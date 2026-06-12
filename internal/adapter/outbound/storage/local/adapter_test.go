package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/service"
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

func TestIsValidExternalID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		// Accept: current SHA3-256 hex format
		{"sha3 lowercase hex", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
		// Accept: legacy IPFS CIDv0 (base58btc, starts with Qm)
		{"ipfs CIDv0", "QmeNLuvfd2yxaXRPDTSb2k9LxspUucqh7dSzJ7pttxKjhD", true},
		{"ipfs CIDv0 mixed case", "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", true},
		// Reject: path traversal & special chars
		{"path traversal", "../etc/passwd", false},
		{"forward slash", "abc/def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"backslash", "abc\\def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"dot", "abc.def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"nul byte", "abc\x00def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"hyphen", "abc-def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		// Reject: length bounds
		{"too short", "abc", false},
		{"empty", "", false},
		{"too long", strings.Repeat("a", 129), false}, // valid chars, just exceeds max length 128
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidExternalID(tc.id); got != tc.want {
				t.Errorf("isValidExternalID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// --- Spec 020 T006: StageWriter contract ---

func TestStage_CommitPublishesWithCorrectIdentity(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	content := []byte("streaming stage content")

	st, err := a.OpenStage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// nothing observable as a permanent object pre-commit (FR-006)
	if entries := publishedBlobs(t, dir); len(entries) != 0 {
		t.Fatalf("blob visible before Commit: %v", entries)
	}
	if _, err := st.Write(content[:7]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(content[7:]); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if want := service.ComputeHash(content); stored.ExternalID != want {
		t.Errorf("externalID = %s, want %s", stored.ExternalID, want)
	}
	if stored.Size != len(content) || !stored.Created {
		t.Errorf("stored = %+v, want size=%d created=true", stored, len(content))
	}
	got, err := a.Read(stored.ExternalID)
	if err != nil || string(got) != string(content) {
		t.Errorf("published content mismatch: %q err=%v", got, err)
	}
	if entries := stagingArtifacts(t, dir); len(entries) != 0 {
		t.Errorf("staging artifact left after Commit: %v", entries)
	}
}

func TestStage_CommitDedupHit(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	content := []byte("dedup me")
	if _, err := a.Save(content); err != nil {
		t.Fatal(err)
	}

	st, _ := a.OpenStage(context.Background())
	_, _ = st.Write(content)
	stored, err := st.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Created {
		t.Error("Created = true on dedup hit, want false")
	}
	if entries := stagingArtifacts(t, dir); len(entries) != 0 {
		t.Errorf("staging artifact left after dedup commit: %v", entries)
	}
}

func TestStage_AbortRemovesArtifactAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	st, _ := a.OpenStage(context.Background())
	_, _ = st.Write([]byte("doomed"))
	if err := st.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := st.Abort(); err != nil {
		t.Errorf("second Abort: %v", err)
	}
	if entries := stagingArtifacts(t, dir); len(entries) != 0 {
		t.Errorf("staging artifact survived Abort: %v", entries)
	}
	if entries := publishedBlobs(t, dir); len(entries) != 0 {
		t.Errorf("aborted stage published a blob: %v", entries)
	}
	if _, err := st.Commit(); err == nil {
		t.Error("Commit after Abort should fail")
	}
}

func TestStage_AbortAfterCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	st, _ := a.OpenStage(context.Background())
	_, _ = st.Write([]byte("keep me"))
	stored, err := st.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Abort(); err != nil {
		t.Errorf("Abort after Commit: %v", err)
	}
	if _, err := a.Read(stored.ExternalID); err != nil {
		t.Errorf("published blob destroyed by post-commit Abort: %v", err)
	}
}

func publishedBlobs(t *testing.T, dir string) []string {
	t.Helper()
	return globNames(t, dir, func(name string) bool { return name[0] != '.' })
}

func stagingArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	return globNames(t, dir, func(name string) bool { return len(name) > 6 && name[:6] == ".stage" })
}

func globNames(t *testing.T, dir string, keep func(string) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if keep(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}
