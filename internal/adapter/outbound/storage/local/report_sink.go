package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// ReportSink implements port.ReportSink on the same mounted volume as the
// blobs, but inside a RESERVED subdirectory — never among them. See
// ReservedReportDir for why that reservation holds by construction.
type ReportSink struct {
	dir string
	// logf receives non-fatal problems (a directory fsync that failed on a
	// network mount, say). Nil is fine — the sink works without a logger.
	logf func(format string, args ...any)
	// dirCreated records that MkdirAll has run; dirSynced that the directory entry has
	// been flushed. They were ONE flag, and collapsing them silently disabled the
	// fsync: ensureDir set it, so syncDirOnce always returned early and the journal's
	// name was never made durable — the exact loss AppendJournal exists to bound.
	// The journal appends one line per record; without this every one of them
	// would re-MkdirAll and re-fsync a directory whose entry only changed on the
	// first line, and on the NFS/FUSE mounts this runs on that is the slowest and
	// most failure-prone step in the call.
	dirCreated bool
	dirSynced  bool
}

// ReservedReportDir is the directory name reports live under, owned here because
// this package owns the key rule that makes it safe. IsBlobName accepts
// only 32-128 alphanumerics, so this name is disqualified twice over — too short,
// and `_`/`-` are outside the alphabet — which is why no blob can ever collide
// with it and no enumeration of the store can mistake a report for content.
const ReservedReportDir = "_sweep-reports"

// NewReportSink roots a sink at <basePath>/<reservedDir>. The directory is
// created on first write, so a service that never sweeps never makes it.
func NewReportSink(basePath, reservedDir string) *ReportSink {
	return &ReportSink{dir: filepath.Join(basePath, reservedDir)}
}

// WithLogf attaches a destination for non-fatal warnings.
func (s *ReportSink) WithLogf(logf func(format string, args ...any)) *ReportSink {
	s.logf = logf
	return s
}

// Dir is the directory reports land in — surfaced so a caller can name it in
// operator-facing output before any report exists.
func (s *ReportSink) Dir() string { return s.dir }

// WriteReport writes data to <dir>/<name> and fsyncs it before returning the
// absolute path. An existing report is never clobbered.
//
// It stages to a temp file and renames into place, rather than creating the final
// name and writing into it. Writing in place would mean a crash mid-write leaves a
// TRUNCATED report under a name indistinguishable from a complete one — and for an
// irreversible migration whose report is the only surviving record, "looks valid
// but is short" is the worst possible failure. Rename is atomic within a
// directory, so the final name only ever appears once the bytes are durable.
func (s *ReportSink) WriteReport(name string, data []byte) (string, error) {
	path, err := s.pathFor(name)
	if err != nil {
		return "", err
	}
	if err := s.ensureDir(); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("write report %s: %w", path, os.ErrExist)
	}

	tmp, err := os.CreateTemp(s.dir, "."+name+".partial-*")
	if err != nil {
		return "", fmt.Errorf("stage report %s: %w", path, err)
	}
	tmpName := tmp.Name()
	if err := writeAndSync(tmp, data); err != nil {
		_ = tmp.Close()
		s.remove(tmpName)
		return "", fmt.Errorf("write report %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		s.remove(tmpName)
		return "", fmt.Errorf("close report %s: %w", path, err)
	}
	// O_EXCL semantics via Link: rename would silently replace an existing report,
	// and reports accumulate across runs.
	if err := os.Link(tmpName, path); err != nil {
		s.remove(tmpName)
		return "", fmt.Errorf("publish report %s: %w", path, err)
	}
	s.remove(tmpName)
	s.syncDirBestEffort(path)
	return s.abs(path), nil
}

// AppendJournal appends line to <dir>/<name>, fsyncing before it returns, so a
// mapping recorded here outlives the process that recorded it.
func (s *ReportSink) AppendJournal(name string, line []byte) (string, error) {
	path, err := s.pathFor(name)
	if err != nil {
		return "", err
	}
	if err := s.ensureDir(); err != nil {
		return "", err
	}
	// #nosec G304 -- path is <reserved dir>/<name>, and name is constrained by
	// pathFor to a bare file name, so it cannot escape the directory this sink owns.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open journal %s: %w", path, err)
	}
	if err := writeAndSync(f, line); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("append journal %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close journal %s: %w", path, err)
	}
	// Only the FIRST append creates a directory entry; the rest extend a file that
	// is already linked, so re-fsyncing the directory per line would flush nothing.
	s.syncDirOnce(path)
	return s.abs(path), nil
}

// ensureDir creates the reserved directory once per sink.
func (s *ReportSink) ensureDir() error {
	if s.dirCreated {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("create report dir %s: %w", s.dir, err)
	}
	// Only MkdirAll is cached here. Whether the entry is DURABLE is a separate
	// question, answered by syncDirOnce.
	s.dirCreated = true
	return nil
}

// syncDirBestEffort flushes the directory entry. Deliberately non-fatal: the file
// data is already durable, and on the NFS/FUSE mounts this runs on a directory
// fsync is the step most likely to fail — reporting the mapping as lost while it
// sits on disk would be a worse error than the one being reported.
func (s *ReportSink) syncDirBestEffort(path string) {
	if err := s.syncDir(); err != nil {
		if s.logf != nil {
			s.logf("report directory fsync failed for %s (the file itself is durable): %v", path, err)
		}
		// dirSynced is NOT set on failure, so a later append retries the one flush
		// that makes the journal's NAME durable.
		return
	}
	s.dirSynced = true
}

func (s *ReportSink) syncDirOnce(path string) {
	if s.dirSynced {
		return
	}
	s.syncDirBestEffort(path)
}

func (s *ReportSink) remove(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) && s.logf != nil {
		s.logf("could not remove the staging file %s: %v", path, err)
	}
}

func (s *ReportSink) abs(path string) string {
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return path // relative, but it still locates the file
}

// pathFor validates the caller's name and resolves it inside the reserved dir.
func (s *ReportSink) pathFor(name string) (string, error) {
	// "." and ".." survive filepath.Base unchanged, and Join would resolve ".."
	// to the parent — outside the reserved directory — so they are rejected by
	// name rather than left to fail on whatever the filesystem happens to do.
	if name == "" || name == "." || name == ".." ||
		name != filepath.Base(name) || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("write report %q: %w", name, port.ErrInvalidKey)
	}
	return filepath.Join(s.dir, name), nil
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// syncDir flushes the reserved directory AND its own entry in the parent.
//
// Syncing s.dir alone makes the entries inside it durable — not the fact that
// s.dir exists. On the first real pass, which is what creates it (dry runs write
// nothing), a crash could therefore unlink the whole directory with every fsynced
// mapping inside, for blobs already reclaimed and rows that no longer match the
// scan predicate. That is precisely the loss AppendJournal exists to bound.
func (s *ReportSink) syncDir() error {
	if err := syncPath(filepath.Dir(s.dir)); err != nil {
		return err
	}
	return syncPath(s.dir)
}

func syncPath(p string) error {
	d, err := os.Open(p) // #nosec G304 -- sink-owned directory paths only
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
