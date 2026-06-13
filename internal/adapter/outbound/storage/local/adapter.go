// Package local implements port.StoragePort on the local filesystem with
// content-addressed blobs: a file's name is the hash of its bytes, so
// identical content dedups to one blob. Uploads stream through a staging
// temp file that is hashed while written and atomically published under its
// content hash on Commit (spec 020) — a no-overwrite link, so concurrent
// commits of identical content can't race. External IDs are validated against
// the exact canonical encodings to keep every path inside the base directory.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// Read returns the whole blob. The wrapped error matches os.ErrNotExist via
// errors.Is when the blob is missing; an invalid externalID is rejected
// before touching the filesystem.
func (a *Adapter) Read(externalID string) ([]byte, error) {
	if !isValidExternalID(externalID) {
		return nil, fmt.Errorf("invalid external ID: %s", externalID)
	}
	data, err := os.ReadFile(a.filePath(externalID))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", externalID, err)
	}
	return data, nil
}

// Delete removes the blob. Idempotent: a missing file is success, so
// concurrent refcount-driven cleanups cannot fail each other.
func (a *Adapter) Delete(externalID string) error {
	if !isValidExternalID(externalID) {
		return fmt.Errorf("invalid external ID: %s", externalID)
	}
	err := os.Remove(a.filePath(externalID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", externalID, err)
	}
	return nil
}

// Exists stats the blob path without opening it. (false, nil) is a
// definitive "not there"; any other stat failure is returned as an error.
func (a *Adapter) Exists(externalID string) (bool, error) {
	if !isValidExternalID(externalID) {
		return false, fmt.Errorf("invalid external ID: %s", externalID)
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

// isValidExternalID validates a storage ID against the exact encodings the
// service produces, so Read/Delete/Exists can only ever address canonical
// content-addressed blobs — never an arbitrary filename under basePath. Two
// formats are accepted:
//   - SHA3-256 hex: exactly 64 lowercase hex chars (current Go file-service
//     format; the output of service.Hasher.Sum).
//   - IPFS CIDv0:   exactly 46 base58btc chars starting with "Qm" (legacy TS
//     file-service).
//
// Restricting to these exact shapes blocks '/', '\', '.', '..', NUL, control
// chars, whitespace, and any non-canonical filename — anything that could
// escape basePath or read a non-blob file.
func isValidExternalID(id string) bool {
	if isSHA3Hex(id) {
		return true
	}
	return isCIDv0(id)
}

// isSHA3Hex reports whether id is exactly 64 lowercase hex characters.
func isSHA3Hex(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// base58btc is the Bitcoin base58 alphabet (excludes 0, O, I and l), used by
// IPFS CIDv0 multihashes.
const base58btc = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isCIDv0 reports whether id is a 46-char IPFS CIDv0: the "Qm" multihash
// prefix followed by 44 base58btc characters.
func isCIDv0(id string) bool {
	if len(id) != 46 || id[0] != 'Q' || id[1] != 'm' {
		return false
	}
	for i := 2; i < len(id); i++ {
		if strings.IndexByte(base58btc, id[i]) < 0 {
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
