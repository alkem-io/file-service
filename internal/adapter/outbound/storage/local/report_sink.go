package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// ReportSink implements port.ReportSink on the same mounted volume as the
// blobs, but inside a RESERVED subdirectory — never among them.
//
// The reservation is safe by construction, not by convention: isValidExternalID
// accepts only 32–128 alphanumeric characters, so a directory named
// `_sweep-reports` is disqualified twice over — too short, and `_`/`-` are
// outside the alphabet. No blob can ever be named this, and no enumeration of
// the blob namespace can mistake a report for content.
type ReportSink struct {
	dir string
	// logf receives non-fatal problems (a directory fsync that failed on a
	// network mount, say). Nil is fine — the sink works without a logger.
	logf func(format string, args ...any)
}

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

// WriteReport writes data to <dir>/<name> exclusively and fsyncs it before
// returning the absolute path. An existing report is never clobbered.
func (s *ReportSink) WriteReport(name string, data []byte) (string, error) {
	return s.write(name, data, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
}

// AppendJournal appends line to <dir>/<name>, fsyncing before it returns, so a
// mapping recorded here outlives the process that recorded it.
func (s *ReportSink) AppendJournal(name string, line []byte) (string, error) {
	return s.write(name, line, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
}

func (s *ReportSink) write(name string, data []byte, flags int) (string, error) {
	path, err := s.pathFor(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return "", fmt.Errorf("create report dir %s: %w", s.dir, err)
	}
	exclusive := flags&os.O_EXCL != 0

	// #nosec G304 -- path is <reserved dir>/<name>, and name is constrained by
	// pathFor to a bare file name, so it cannot escape the directory this sink owns.
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return "", fmt.Errorf("open report %s: %w", path, err)
	}
	if err := writeAndSync(f, data); err != nil {
		_ = f.Close()
		s.discardPartial(path, exclusive)
		return "", fmt.Errorf("write report %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		s.discardPartial(path, exclusive)
		return "", fmt.Errorf("close report %s: %w", path, err)
	}

	// Syncing the file alone can leave its directory entry unflushed. Best-effort
	// on purpose: the data is already durable here, and on the NFS/FUSE mounts
	// this service runs on a directory fsync is the step most likely to fail.
	// Failing the call would tell an operator the mapping was lost while it sits
	// on disk — a worse error than the one being reported.
	if err := s.syncDir(); err != nil && s.logf != nil {
		s.logf("report directory fsync failed for %s (the file itself is durable): %v", path, err)
	}

	if abs, err := filepath.Abs(path); err == nil {
		return abs, nil
	}
	return path, nil // relative, but it still locates the file
}

// discardPartial removes a file this call created but could not finish. A
// half-written file whose name looks exactly like a valid report is worse than
// no file: whoever later globs the directory cannot tell them apart. An APPEND
// is left alone — its earlier lines are still the record.
func (s *ReportSink) discardPartial(path string, created bool) {
	if !created {
		return
	}
	if err := os.Remove(path); err != nil && s.logf != nil {
		s.logf("could not remove the partially-written report %s: %v", path, err)
	}
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

func (s *ReportSink) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
