package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alkem-io/file-service-go/internal/domain/service"
)

func TestSave_StoresFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	content := []byte("hello world")
	stored, err := a.Save(content)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	expectedHash := service.ComputeHash(content)
	if stored.ExternalID != expectedHash {
		t.Errorf("externalID = %q, want %q", stored.ExternalID, expectedHash)
	}
	if stored.Size != len(content) {
		t.Errorf("size = %d, want %d", stored.Size, len(content))
	}

	// Verify file exists on disk
	data, err := os.ReadFile(filepath.Join(dir, expectedHash)) //nolint:gosec // test reads from temp dir
	if err != nil {
		t.Fatalf("file not on disk: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("file content mismatch")
	}
}

func TestSave_Dedup(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	content := []byte("same content")
	s1, err := a.Save(content)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := a.Save(content)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ExternalID != s2.ExternalID {
		t.Errorf("dedup failed: %q != %q", s1.ExternalID, s2.ExternalID)
	}
}

func TestSave_DifferentContent(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	s1, _ := a.Save([]byte("content A"))
	s2, _ := a.Save([]byte("content B"))
	if s1.ExternalID == s2.ExternalID {
		t.Error("different content produced same hash")
	}
}

func TestRead_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	content := []byte("read me")
	stored, _ := a.Save(content)

	data, err := a.Read(stored.ExternalID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: %q != %q", data, content)
	}
}

func TestRead_MissingFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	// Valid hex format but file does not exist on disk
	_, err := a.Read("0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestRead_InvalidExternalID(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	_, err := a.Read("../etc/passwd")
	if err == nil {
		t.Fatal("expected error for invalid external ID")
	}
}

func TestDelete_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	stored, _ := a.Save([]byte("delete me"))
	err := a.Delete(stored.ExternalID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	exists, _ := a.Exists(stored.ExternalID)
	if exists {
		t.Error("file still exists after delete")
	}
}

func TestDelete_NonExistent(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	// Valid hex format but file does not exist — should not error
	err := a.Delete("0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatalf("Delete non-existent should not error: %v", err)
	}
}

func TestDelete_InvalidExternalID(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	err := a.Delete("../etc/passwd")
	if err == nil {
		t.Fatal("expected error for invalid external ID")
	}
}

func TestSave_InvalidDir(t *testing.T) {
	// Use a path that can't be created (file as directory)
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	a := New(tmpFile) // basePath is a file, not dir

	_, err := a.Save([]byte("content"))
	if err == nil {
		t.Fatal("expected error when basePath is a file")
	}
}

func TestSave_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	// First save succeeds (creates the file)
	_, err := a.Save([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}

	// Make dir read-only
	if err := os.Chmod(dir, 0o444); err != nil { //nolint:gosec // test needs read-only dir
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o750) }() //nolint:gosec // restore for cleanup

	// Second save with different content should fail (can't create temp file)
	_, err = a.Save([]byte("second-different"))
	if err == nil {
		t.Fatal("expected error on read-only dir")
	}
}

func TestDelete_PermissionDenied(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	stored, _ := a.Save([]byte("protected"))

	// Make dir read-only so Remove fails
	if err := os.Chmod(dir, 0o444); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o750) }() //nolint:gosec // restore

	err := a.Delete(stored.ExternalID)
	if err == nil {
		t.Fatal("expected error for permission denied delete")
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	stored, _ := a.Save([]byte("exists check"))

	exists, err := a.Exists(stored.ExternalID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected file to exist")
	}

	// Valid hex format but file does not exist
	exists, err = a.Exists("0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected file to not exist")
	}
}

func TestExists_InvalidExternalID(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)

	_, err := a.Exists("../etc/passwd")
	if err == nil {
		t.Fatal("expected error for invalid external ID")
	}
}
