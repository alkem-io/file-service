// Package local implements port.StoragePort on the local filesystem with
// content-addressed blobs: a file's name is the hash of its bytes, so
// identical content dedups to one blob. Uploads stream through a staging
// temp file that is hashed while written and atomically published under its
// content hash on Commit (spec 020) — a no-overwrite link, so concurrent
// commits of identical content can't race. External IDs are validated against
// a deliberately permissive allow-list (alphanumeric, bounded length) so that
// both current SHA3-256 hex names and legacy IPFS CID names resolve, while
// path-traversal characters are kept out of the base directory.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// Adapter implements port.StoragePort using the local filesystem.
type Adapter struct {
	basePath string
}

// New creates an Adapter rooted at basePath. The directory is not created
// here — OpenStage creates it lazily on the first write.
func New(basePath string) *Adapter {
	return &Adapter{basePath: basePath}
}

// Save stores a complete buffer. Implemented on top of the streaming stage
// (spec 020) so there is exactly one write/publish code path per backend.
func (a *Adapter) Save(content []byte) (model.StoredFile, error) {
	st, err := a.OpenStage(context.Background())
	if err != nil {
		return model.StoredFile{}, err
	}
	if _, err := st.Write(content); err != nil {
		_ = st.Abort()
		return model.StoredFile{}, err
	}
	stored, err := st.Commit()
	if err != nil {
		_ = st.Abort()
		return model.StoredFile{}, err
	}
	return stored, nil
}

// Read returns the whole blob. A missing blob wraps os.ErrNotExist (errors.Is);
// an invalid externalID returns port.ErrInvalidKey before touching the filesystem.
// It does NOT try to tell a genuinely-absent blob from a store-wide outage — an
// empty basePath is inherently ambiguous (unmounted vs never-written), so that
// judgement belongs to the caller that knows what SHOULD exist (the backup worker
// cross-checks the alkemio `file` corpus on a 404); file-service stays a dumb blob
// server. A non-ENOENT backend failure (e.g. an NFS EIO/ESTALE outage) is returned
// as-is → the handler answers 500 (retryable), never a false 404.
func (a *Adapter) Read(externalID string) ([]byte, error) {
	if !IsBlobName(externalID) {
		return nil, fmt.Errorf("read %q: %w", externalID, port.ErrInvalidKey)
	}
	data, err := os.ReadFile(a.filePath(externalID))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", externalID, err)
	}
	return data, nil
}

// ReadStream opens the blob for streaming so the caller never holds the whole
// blob in memory. The returned *os.File is the io.ReadCloser; size comes from
// the same open handle's Stat (no TOCTOU against a concurrent replace, which
// publishes a NEW hash under a NEW name — this name is immutable once written).
// Same error contract as Read.
func (a *Adapter) ReadStream(externalID string) (io.ReadCloser, int64, error) {
	if !IsBlobName(externalID) {
		return nil, 0, fmt.Errorf("read %q: %w", externalID, port.ErrInvalidKey)
	}
	f, err := os.Open(a.filePath(externalID))
	if err != nil {
		return nil, 0, fmt.Errorf("read file %s: %w", externalID, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat blob %s: %w", externalID, err)
	}
	if fi.IsDir() {
		// os.Open + Stat both succeed on a directory (unlike Read's os.ReadFile, which
		// EISDIRs). Guard it here so a directory at a blob path fails BEFORE the handler
		// commits a 200 + Content-Length and only io.Copy discovers EISDIR mid-stream.
		_ = f.Close()
		return nil, 0, fmt.Errorf("read %q: blob path is a directory, not a file", externalID)
	}
	return f, fi.Size(), nil
}

// Delete removes the blob. Idempotent: a missing file is success, so
// concurrent refcount-driven cleanups cannot fail each other.
func (a *Adapter) Delete(externalID string) error {
	if !IsBlobName(externalID) {
		return fmt.Errorf("%q: %w", externalID, port.ErrInvalidKey)
	}
	err := os.Remove(a.filePath(externalID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", externalID, err)
	}
	return nil
}

// Exists stats the blob path without opening it. (false, nil) is a
// definitive "not there"; any other stat failure is returned as an error.
// sha3HexName matches the current content-addressing scheme: lowercase SHA3-256
// hex. Anything else on the volume is legacy-named and in scope for the sweep.
var sha3HexName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ListLegacyNamed streams the flat blob namespace once and returns the names that
// are not the current scheme.
//
// d.ReadDir(n) reads a CHUNK per call, unlike os.ReadDir, which materializes and
// sorts every entry — on a production store of hundreds of thousands of blobs that
// is the difference between peak memory scaling with the matches and with the whole
// corpus. Legacy names are rare, so the returned slice stays small.
//
// Reserved sidecar directories are excluded by taking files only at depth 1 and
// skipping `_`/`.` prefixes — the same rule that makes those names unreachable as
// blob keys.
func (a *Adapter) ListLegacyNamed(limit int) ([]string, error) {
	// Rejected before opening the directory: the `len(names) == limit` check below
	// can never trip for a non-positive limit, so the walk would retain every match
	// in the corpus — precisely the unbounded growth this one-pass form replaced.
	if limit <= 0 {
		return nil, fmt.Errorf("list legacy names: limit must be positive, got %d", limit)
	}
	d, err := os.Open(a.basePath)
	if err != nil {
		// A missing root is NOT an empty corpus. Reporting it as one lets an
		// unmounted volume read as a fully-swept store.
		return nil, fmt.Errorf("open %s: %w", a.basePath, err)
	}
	defer func() { _ = d.Close() }()

	var names []string
	for {
		entries, err := d.ReadDir(512)
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", a.basePath, err)
		}
		for _, e := range entries {
			n := e.Name()
			// IsBlobName, not just "not a sidecar": the storage-checkup job leaves
			// files_list.txt and table_entries.txt in this directory after an eviction,
			// and those are neither dot-prefixed nor 64-hex. Enumerating them makes the
			// sweep WARN "needs manual repair" about another job's temp files and
			// inflates the dry-run count an operator approves the pass against.
			if e.IsDir() || !IsBlobName(n) || sha3HexName.MatchString(n) {
				continue
			}
			names = append(names, n)
			if len(names) == limit {
				return names, nil
			}
		}
	}
}

// Link publishes existing bytes under a second name without copying them. A
// no-overwrite hard link, so a target that already exists is reported as a dedup
// hit rather than clobbered.
func (a *Adapter) Link(existing, newName string) (bool, error) {
	if !IsBlobName(existing) || !IsBlobName(newName) {
		return false, fmt.Errorf("link %q -> %q: %w", existing, newName, port.ErrInvalidKey)
	}
	if err := os.Link(a.filePath(existing), a.filePath(newName)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil // dedup hit: the bytes are already published
		}
		return false, fmt.Errorf("link %s -> %s: %w", existing, newName, err)
	}
	return true, nil
}

// SameFile reports whether two names resolve to the same underlying file, by inode
// identity rather than by spelling. os.SameFile is the only thing that can answer it:
// on a case-insensitive volume two spellings share one inode; on a case-sensitive one
// they are separate files that may both exist.
func (a *Adapter) SameFile(x, y string) (bool, error) {
	if !IsBlobName(x) || !IsBlobName(y) {
		return false, fmt.Errorf("same-file %q/%q: %w", x, y, port.ErrInvalidKey)
	}
	xi, err := os.Stat(a.filePath(x))
	if err != nil {
		return false, err
	}
	yi, err := os.Stat(a.filePath(y))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // y is not there at all, so they cannot be one file
		}
		return false, err
	}
	return os.SameFile(xi, yi), nil
}

// ReservedParkedDir holds blobs a migration moved aside. Same construction as
// ReservedReportDir: IsBlobName accepts only 32-128 alphanumerics, so this
// name is disqualified twice over — too short, and `_` is outside the alphabet.
const ReservedParkedDir = "_parked"

// Park moves a blob into ReservedParkedDir instead of unlinking it. os.Rename
// within one filesystem is atomic and copies no data. Parking a name that is
// already absent is not an error, so a re-run after an interruption is safe.
func (a *Adapter) Park(externalID string) (string, error) {
	if !IsBlobName(externalID) {
		return "", fmt.Errorf("park %q: %w", externalID, port.ErrInvalidKey)
	}
	src := a.filePath(externalID)
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	dir := filepath.Join(a.basePath, ReservedParkedDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create parked dir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, externalID)
	// os.Rename replaces an existing destination on Linux, which is safe here because
	// the destination cannot already exist: parking removes the name from the content
	// namespace, and nothing can re-create it — the only writer there is Commit, which
	// publishes under a SHA3 digest, never under a legacy name. A second pass over the
	// same name therefore stats the source, finds it gone, and returns ("", nil) above.
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("park %s: %w", externalID, err)
	}
	return dst, nil
}

// Exists reports whether a blob is present without reading its bytes.
func (a *Adapter) Exists(externalID string) (bool, error) {
	if !IsBlobName(externalID) {
		return false, fmt.Errorf("%q: %w", externalID, port.ErrInvalidKey)
	}
	_, err := os.Stat(a.filePath(externalID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (a *Adapter) filePath(externalID string) string {
	return filepath.Join(a.basePath, externalID)
}

// IsBlobName reports whether a name is one this store can address. It is the ONE
// definition of "looks like content" — the sweep's pre-flight calls it rather than
// carrying a copy, so the two cannot drift into a pre-flight that accepts names
// enumeration ignores.
//
// It rejects path-traversal characters and is *deliberately* permissive about hash
// encoding: any alphanumeric (ASCII) name of 32–128 characters. Real names on disk
// include CIDv0 base58btc (`Qm…`), CIDv1 base32 (`bafy…`), uppercase hex, and current
// SHA3-256 hex.
//
// DO NOT re-tighten this to exact formats. It was widened by PR #13 ("Accept legacy
// IPFS CID externalIDs in storage validator") because narrowing it broke production
// reads. That cost is now higher, not lower: this is exported and the sweep's
// pre-flight is a second caller.
func IsBlobName(id string) bool {
	if len(id) < 32 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// stage is the filesystem StageWriter: a temp file in the blob directory,
// hashed while written, atomically published under its content hash on Commit
// (spec 020).
type stage struct {
	a         *Adapter
	tmp       *os.File
	tmpName   string
	hasher    *service.Hasher
	size      int
	committed bool
	aborted   bool
}

// OpenStage implements port.StoragePort (spec 020).
func (a *Adapter) OpenStage(_ context.Context) (port.StageWriter, error) {
	if err := os.MkdirAll(a.basePath, 0o750); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	tmp, err := os.CreateTemp(a.basePath, ".stage-*")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	return &stage{a: a, tmp: tmp, tmpName: tmp.Name(), hasher: service.NewHasher()}, nil
}

func (s *stage) Write(p []byte) (int, error) {
	if s.committed || s.aborted {
		return 0, fmt.Errorf("stage: write after commit/abort")
	}
	n, err := s.tmp.Write(p)
	if n > 0 {
		_, _ = s.hasher.Write(p[:n])
		s.size += n
	}
	if err != nil {
		return n, fmt.Errorf("write staging file: %w", err)
	}
	return n, nil
}

// StagedReaderAt exposes the staging temp file for random-access inspection
// before Commit. An *os.File already satisfies io.ReaderAt, and ReadAt is
// offset-based so the file's seek position is irrelevant — the writer never
// seeks it. The staged content is flushed to disk (Sync) before the reader is
// returned. Calling this after Commit or Abort is an error: the temp file no
// longer exists.
func (s *stage) StagedReaderAt() (io.ReaderAt, int64, error) {
	if s.committed || s.aborted {
		return nil, 0, fmt.Errorf("stage: reader after commit/abort")
	}
	if err := s.tmp.Sync(); err != nil {
		return nil, 0, fmt.Errorf("sync staging file: %w", err)
	}
	return s.tmp, int64(s.size), nil
}

// Commit closes the staging file and publishes it under its content hash.
// Publish is atomic and never overwrites an existing blob: os.Link creates
// the content-addressed name in a single step that fails with os.IsExist when
// the blob is already present. That collapses the previous stat+rename window
// in which two concurrent commits of identical content could both observe a
// stat miss and the second's rename would overwrite, wrongly returning
// Created=true. With the link, exactly one commit wins (Created=true) and any
// other observing os.IsExist takes the dedup path (Created=false) — the blob
// is byte-identical by construction, so the loser's staging file is simply
// discarded. Calling Commit twice, or after Abort, is an error.
func (s *stage) Commit() (model.StoredFile, error) {
	if s.committed {
		return model.StoredFile{}, fmt.Errorf("stage: double commit")
	}
	if s.aborted {
		return model.StoredFile{}, fmt.Errorf("stage: commit after abort")
	}
	if err := s.tmp.Close(); err != nil {
		return model.StoredFile{}, fmt.Errorf("close staging file: %w", err)
	}

	externalID := s.hasher.Sum()
	finalPath := s.a.filePath(externalID)

	created, err := s.publish(finalPath)
	if err != nil {
		return model.StoredFile{}, err
	}
	s.committed = true
	return model.StoredFile{ExternalID: externalID, Size: s.size, Created: created}, nil
}

// publish atomically links the staging file to finalPath and removes the
// staging file. It returns created=true when this commit created the blob and
// created=false on a dedup hit (finalPath already existed). The staging file
// is always removed on success.
func (s *stage) publish(finalPath string) (created bool, err error) {
	switch linkErr := os.Link(s.tmpName, finalPath); {
	case linkErr == nil:
		created = true
	case os.IsExist(linkErr):
		created = false
	case errors.Is(linkErr, syscall.EXDEV):
		// Staging dir and blob dir are on different filesystems (hard links
		// can't cross devices). Fall back to an atomic O_EXCL create+copy,
		// which preserves the same no-overwrite, race-safe semantics.
		return s.publishCrossDevice(finalPath)
	default:
		return false, fmt.Errorf("publish staged file: %w", linkErr)
	}

	if rmErr := os.Remove(s.tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
		// The blob is safely published; only the staging artifact leaked.
		// Surface it rather than accumulate orphans.
		return created, fmt.Errorf("remove staging file after publish: %w", rmErr)
	}
	return created, nil
}

// publishCrossDevice handles the EXDEV fallback: copy the staging bytes into
// an O_EXCL-created final file (so a concurrent commit cannot be overwritten),
// treating an existing blob as a dedup hit. The staging file is removed on
// success.
func (s *stage) publishCrossDevice(finalPath string) (created bool, err error) {
	src, err := os.Open(s.tmpName) //nolint:gosec // s.tmpName is created by OpenStage under basePath
	if err != nil {
		return false, fmt.Errorf("reopen staging file: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(finalPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) //nolint:gosec // finalPath = basePath + content hash (s.hasher.Sum); no caller input
	if err != nil {
		if os.IsExist(err) {
			if rmErr := os.Remove(s.tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
				return false, fmt.Errorf("remove staging file on dedup: %w", rmErr)
			}
			return false, nil
		}
		return false, fmt.Errorf("create blob file: %w", err)
	}

	if _, copyErr := io.Copy(dst, src); copyErr != nil {
		_ = dst.Close()
		_ = os.Remove(finalPath)
		return false, fmt.Errorf("copy staged file across devices: %w", copyErr)
	}
	if closeErr := dst.Close(); closeErr != nil {
		_ = os.Remove(finalPath)
		return false, fmt.Errorf("close blob file: %w", closeErr)
	}
	if rmErr := os.Remove(s.tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
		return true, fmt.Errorf("remove staging file after publish: %w", rmErr)
	}
	return true, nil
}

// Abort closes and removes the staging file. Safe to call at any point —
// a no-op after a successful Commit or a prior Abort, and after a failed
// Commit it cleans up the leftover staging file — so callers can defer it
// unconditionally.
func (s *stage) Abort() error {
	if s.aborted || s.committed {
		return nil
	}
	s.aborted = true
	_ = s.tmp.Close()
	if err := os.Remove(s.tmpName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove staging file: %w", err)
	}
	return nil
}
