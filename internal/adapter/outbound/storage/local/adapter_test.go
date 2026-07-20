package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
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
		// The validator is deliberately permissive about hash encoding: it must
		// accept both current SHA3-256 hex names and any legacy IPFS CID name
		// (CIDv0/CIDv1, uppercase hex, etc.) the pre-migration TS file-service
		// wrote to disk — see PR #13. Its real contract is rejecting
		// path-traversal characters and bounding length, NOT pinning the
		// encoding. Do NOT re-add "exact format" rejections here.

		// Accept: current SHA3-256 hex format (64 lowercase hex chars).
		{"sha3 lowercase hex", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", true},
		// Accept: legacy IPFS CIDv0 (base58btc, starts with Qm).
		{"ipfs CIDv0", "QmeNLuvfd2yxaXRPDTSb2k9LxspUucqh7dSzJ7pttxKjhD", true},
		{"ipfs CIDv0 mixed case", "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG", true},
		// Accept: legacy IPFS CIDv1 (base32 "bafy…").
		{"ipfs CIDv1 base32", "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi", true},
		// Accept: uppercase hex (legacy externalIDs were not lowercase-pinned).
		{"uppercase hex", "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", true},
		// Accept: alphanumeric of various non-64-hex lengths within bounds.
		{"alphanumeric 32 chars", "abcdef0123456789ABCDEF0123456789", true},
		// Reject: path traversal & special chars (the real security boundary).
		{"path traversal", "../etc/passwd", false},
		{"forward slash", "abc/def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"backslash", "abc\\def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"dot", "abc.def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"nul byte", "abc\x00def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"control char", "abc\x01def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"hyphen", "abc-def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		{"whitespace", "abc def0123456789abcdef0123456789abcdef0123456789abcdef0123456789", false},
		// Reject: length bounds (<32, >128).
		{"too short", "abc", false},
		{"empty", "", false},
		{"too long", strings.Repeat("a", 129), false},
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

func TestStage_StagedReaderAtReturnsStagedBytes(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	content := []byte("staged bytes for random-access inspection")

	st, err := a.OpenStage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(content[:10]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(content[10:]); err != nil {
		t.Fatal(err)
	}

	ra, size, err := st.StagedReaderAt()
	if err != nil {
		t.Fatalf("StagedReaderAt: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
	got := make([]byte, size)
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("staged bytes = %q, want %q", got, content)
	}

	// After Commit the staging artifact is gone: StagedReaderAt must error.
	if _, err := st.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.StagedReaderAt(); err == nil {
		t.Error("StagedReaderAt after Commit = nil error, want error")
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

// TestStage_ConcurrentCommitsExactlyOneCreated defends the StageWriter
// contract under concurrency: when several stages of byte-identical content
// commit at once, the atomic publish must report Created=true exactly once and
// Created=false for every dedup; all must agree on the externalID and leave a
// single blob plus no staging artifacts.
func TestStage_ConcurrentCommitsExactlyOneCreated(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	content := []byte("concurrent identical content")

	const n = 8
	stages := make([]port.StageWriter, n)
	for i := range stages {
		st, err := a.OpenStage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write(content); err != nil {
			t.Fatal(err)
		}
		stages[i] = st
	}

	results := make([]model.StoredFile, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range stages {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = stages[i].Commit()
		}(i)
	}
	wg.Wait()

	createdCount := 0
	want := service.ComputeHash(content)
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("commit %d failed: %v", i, errs[i])
		}
		if results[i].ExternalID != want {
			t.Errorf("commit %d externalID = %q, want %q", i, results[i].ExternalID, want)
		}
		if results[i].Created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Errorf("Created=true reported %d times, want exactly 1", createdCount)
	}
	if blobs := publishedBlobs(t, dir); len(blobs) != 1 {
		t.Errorf("published blobs = %v, want exactly 1", blobs)
	}
	if arts := stagingArtifacts(t, dir); len(arts) != 0 {
		t.Errorf("staging artifacts left after concurrent commits: %v", arts)
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

// ReadStream returns the blob bytes + size and rejects a traversal key with ErrInvalidKey.
func TestReadStream(t *testing.T) {
	a := New(t.TempDir())
	stored, err := a.Save([]byte("stream me"))
	if err != nil {
		t.Fatal(err)
	}
	rc, size, err := a.ReadStream(stored.ExternalID)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if size != int64(len("stream me")) {
		t.Errorf("size = %d, want %d", size, len("stream me"))
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "stream me" {
		t.Errorf("body = %q", b)
	}

	if _, _, err := a.ReadStream("../etc/passwd"); !errors.Is(err, port.ErrInvalidKey) {
		t.Errorf("traversal key: err = %v, want ErrInvalidKey", err)
	}
}

// A genuinely absent blob is os.ErrNotExist, for both Read and ReadStream. file-service does
// not try to distinguish absence from a store outage (an empty root is ambiguous); that call
// is the worker's, via the corpus cross-check.
func TestReadAbsentBlobIsNotExist(t *testing.T) {
	valid := "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a"
	a := New(t.TempDir())
	if _, err := a.Read(valid); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read absent blob: err = %v, want os.ErrNotExist", err)
	}
	if _, _, err := a.ReadStream(valid); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadStream absent blob: err = %v, want os.ErrNotExist", err)
	}
}
