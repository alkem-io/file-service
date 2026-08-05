package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/port"
)

const reservedDir = "_sweep-reports"

// A report is the only surviving record of what an irreversible migration did,
// so it has to land in the reserved directory — never among the blobs, whose
// namespace is flat and would count it as content.
func TestReportSink_WritesIntoTheReservedDirectory(t *testing.T) {
	base := t.TempDir()
	sink := NewReportSink(base, reservedDir)

	path, err := sink.WriteReport("sweep-cids-run.json", []byte(`{"schemaVersion":1}`))
	if err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("returned %q; an operator reading only the Job logs needs an absolute path", path)
	}
	if want := filepath.Join(base, reservedDir, "sweep-cids-run.json"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	// #nosec G304 -- path is exactly what the sink under test just returned.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != `{"schemaVersion":1}` {
		t.Errorf("content = %q", got)
	}

	// The reserved name is one the store's own key rules can never produce, so no
	// enumeration of the blob namespace can mistake a report for content.
	if isValidExternalID(reservedDir) {
		t.Errorf("%q passes isValidExternalID — a blob could be named this and the reservation is not safe by construction", reservedDir)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("report sink left %q beside the blobs", e.Name())
		}
	}
}

// Reports accumulate across runs; silently overwriting one would destroy the
// only record of an earlier pass.
func TestReportSink_RefusesToOverwrite(t *testing.T) {
	sink := NewReportSink(t.TempDir(), reservedDir)
	if _, err := sink.WriteReport("run.json", []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := sink.WriteReport("run.json", []byte("second")); !errors.Is(err, os.ErrExist) {
		t.Errorf("second write err = %v, want an ErrExist — an existing report must never be clobbered", err)
	}
}

// The sink's directory is a boundary, not a suggestion.
func TestReportSink_RejectsNamesThatEscapeTheDirectory(t *testing.T) {
	base := t.TempDir()
	sink := NewReportSink(base, reservedDir)

	for _, name := range []string{"", "../escaped.json", "nested/run.json", ".", ".."} {
		path, err := sink.WriteReport(name, []byte("x"))
		if !errors.Is(err, port.ErrInvalidKey) {
			t.Errorf("WriteReport(%q) err = %v, want ErrInvalidKey (wrote to %q)", name, err, path)
		}
	}
	// Nothing escaped, and nothing was created outside the reserved directory.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != reservedDir {
			t.Errorf("unexpected entry %q under the storage root", e.Name())
		}
	}
}

// The sink creates its directory on first write, so a service that never sweeps
// never leaves an empty directory on the content volume.
func TestReportSink_CreatesDirectoryLazily(t *testing.T) {
	base := t.TempDir()
	sink := NewReportSink(base, reservedDir)

	if _, err := os.Stat(sink.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reserved directory exists before any report was written (err=%v)", err)
	}
	if !strings.HasPrefix(sink.Dir(), base) {
		t.Errorf("Dir() = %q, want it under %q", sink.Dir(), base)
	}
	if _, err := sink.WriteReport("run.json", []byte("x")); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if _, err := os.Stat(sink.Dir()); err != nil {
		t.Errorf("reserved directory missing after a write: %v", err)
	}
}
