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
}

// NewReportSink roots a sink at <basePath>/<reservedDir>. The directory is
// created on first write, so a service that never sweeps never makes it.
func NewReportSink(basePath, reservedDir string) *ReportSink {
	return &ReportSink{dir: filepath.Join(basePath, reservedDir)}
}

// Dir is the directory reports land in — surfaced so a caller can name it in
// operator-facing output before any report exists.
func (s *ReportSink) Dir() string { return s.dir }

// WriteReport writes data to <dir>/<name> and fsyncs both the file and the
// directory before returning, so the report survives the node loss that a
// one-shot Job is most likely to be killed by. Returns the absolute path.
func (s *ReportSink) WriteReport(name string, data []byte) (string, error) {
	// "." and ".." survive filepath.Base unchanged, and Join would resolve ".."
	// to the parent — outside the reserved directory — so they are rejected by
	// name rather than left to fail on whatever the filesystem happens to do.
	if name == "" || name == "." || name == ".." ||
		name != filepath.Base(name) || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("write report %q: %w", name, port.ErrInvalidKey)
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return "", fmt.Errorf("create report dir %s: %w", s.dir, err)
	}

	path := filepath.Join(s.dir, name)
	// #nosec G304 -- path is <reserved dir>/<name>, and name is constrained above to a
	// bare file name, so it cannot escape the directory this sink owns.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create report %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write report %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("sync report %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close report %s: %w", path, err)
	}

	// Syncing the file alone can still leave its directory entry unflushed, so
	// the report would exist with no name pointing at it.
	d, err := os.Open(s.dir)
	if err != nil {
		return "", fmt.Errorf("open report dir %s: %w", s.dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return "", fmt.Errorf("sync report dir %s: %w", s.dir, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil //nolint:nilerr // the report IS written; a relative path still locates it
	}
	return abs, nil
}
