package local

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

func storageCID(ch string) string { return "Qm" + strings.Repeat(ch, 44) }

func writeBlob(t *testing.T, root, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBlob(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // test paths are rooted in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func hasExactEntry(t *testing.T, root, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return true
		}
	}
	return false
}

func TestCIDMigrationStorage_PreparesNewTargetAndParksOnlyObsoleteAlias(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("a")
	content := []byte("legacy content")
	target := service.ComputeHash(content)
	unrelated := storageCID("z")
	writeBlob(t, root, cid, content)
	writeBlob(t, root, unrelated, []byte("unrelated"))
	a := NewCIDMigration(root)

	prepared, err := a.PrepareCIDTarget(t.Context(), cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Created || len(prepared.ObsoleteAliases) != 1 || prepared.ObsoleteAliases[0].Name != cid {
		t.Fatalf("prepared = %+v", prepared)
	}
	if got := readBlob(t, filepath.Join(root, target)); string(got) != string(content) {
		t.Fatalf("target = %q", got)
	}
	if err := a.FinalizeCIDAlias(t.Context(), prepared.ObsoleteAliases[0], target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, cid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CID remains active: %v", err)
	}
	if got := readBlob(t, filepath.Join(root, "_parked", "ipfs-cid-sweep", cid)); string(got) != string(content) {
		t.Fatalf("parked CID = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, unrelated)); err != nil {
		t.Fatalf("unrelated blob moved: %v", err)
	}
}

func TestCIDMigrationStorage_VerifiesAndReusesExistingLowercaseTarget(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("b")
	content := []byte("deduplicated")
	target := service.ComputeHash(content)
	writeBlob(t, root, cid, content)
	writeBlob(t, root, target, content)

	prepared, err := NewCIDMigration(root).PrepareCIDTarget(t.Context(), cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Created || len(prepared.ObsoleteAliases) != 1 || prepared.ObsoleteAliases[0].Name != cid {
		t.Fatalf("prepared = %+v", prepared)
	}
}

func TestCIDMigrationStorage_CaseSensitiveVariantsBecomeOrdinaryAliases(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("c")
	content := []byte("case variants")
	target := service.ComputeHash(content)
	upper := strings.ToUpper(target)
	writeBlob(t, root, cid, content)
	writeBlob(t, root, upper, content)
	if _, err := os.Stat(filepath.Join(root, target)); err == nil {
		t.Skip("test requires a case-sensitive filesystem")
	}

	prepared, err := NewCIDMigration(root).PrepareCIDTarget(t.Context(), cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Created || len(prepared.ObsoleteAliases) != 2 {
		t.Fatalf("prepared = %+v", prepared)
	}
	for _, alias := range prepared.ObsoleteAliases {
		if alias.CanonicalizeCase {
			t.Fatalf("case-sensitive alias marked for case canonicalization: %+v", alias)
		}
	}
}

func TestCIDMigrationStorage_RejectsWrongCaseVariantContentBeforePublishing(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("d")
	content := []byte("source")
	target := service.ComputeHash(content)
	writeBlob(t, root, cid, content)
	writeBlob(t, root, strings.ToUpper(target), []byte("wrong"))

	_, err := NewCIDMigration(root).PrepareCIDTarget(t.Context(), cid, target)
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	if hasExactEntry(t, root, target) {
		t.Fatal("exact lowercase target published despite mismatch")
	}
}

type targetAppearsOps struct {
	fileOps
	content []byte
}

type blockingOpenOps struct {
	fileOps
	reader io.ReadCloser
}

func (o blockingOpenOps) open(string) (io.ReadCloser, error) {
	return o.reader, nil
}

type blockingReadCloser struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.closed
	return 0, errors.New("reader closed")
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

type countingReadDirOps struct {
	fileOps
	calls int
}

func (o *countingReadDirOps) readDir(name string) ([]os.DirEntry, error) {
	o.calls++
	return o.fileOps.readDir(name)
}

func TestCIDMigrationStorage_IndexesTargetVariantsOnce(t *testing.T) {
	root := t.TempDir()
	cidA, cidB := storageCID("g"), storageCID("h")
	contentA, contentB := []byte("first indexed target"), []byte("second indexed target")
	writeBlob(t, root, cidA, contentA)
	writeBlob(t, root, cidB, contentB)
	ops := &countingReadDirOps{fileOps: osFileOps{}}
	a := newCIDMigration(root, ops)

	if _, err := a.PrepareCIDTarget(t.Context(), cidA, service.ComputeHash(contentA)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PrepareCIDTarget(t.Context(), cidB, service.ComputeHash(contentB)); err != nil {
		t.Fatal(err)
	}
	if ops.calls != 1 {
		t.Fatalf("storage root read %d times, want once", ops.calls)
	}
}

func (o targetAppearsOps) link(_, newname string) error {
	if err := os.WriteFile(newname, o.content, 0o600); err != nil {
		return err
	}
	return os.ErrExist
}

func TestCIDMigrationStorage_VerifiesTargetThatAppearsDuringPublish(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("f")
	content := []byte("concurrent current-format publication")
	target := service.ComputeHash(content)
	writeBlob(t, root, cid, content)
	a := newCIDMigration(root, targetAppearsOps{fileOps: osFileOps{}, content: content})

	prepared, err := a.PrepareCIDTarget(t.Context(), cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Created {
		t.Fatalf("appeared target reported as newly created: %+v", prepared)
	}
	if got := readBlob(t, filepath.Join(root, target)); string(got) != string(content) {
		t.Fatalf("target = %q", got)
	}
}

func TestCIDMigrationStorage_CancellationInterruptsTargetVerification(t *testing.T) {
	reader := newBlockingReadCloser()
	a := newCIDMigration(t.TempDir(), blockingOpenOps{fileOps: osFileOps{}, reader: reader})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.verifyDigest(ctx, "target-alias", strings.Repeat("a", 64))
	}()

	<-reader.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("verifyDigest error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("verifyDigest did not stop after cancellation")
	}
}

type caseFoldStatOps struct{ base fileOps }

func (o caseFoldStatOps) readDir(name string) ([]os.DirEntry, error) {
	return o.base.readDir(name)
}

func (o caseFoldStatOps) mkdirAll(path string, perm os.FileMode) error {
	return o.base.mkdirAll(path, perm)
}

func (o caseFoldStatOps) link(oldname, newname string) error {
	return o.base.link(oldname, newname)
}

func (o caseFoldStatOps) rename(oldpath, newpath string) error {
	return o.base.rename(oldpath, newpath)
}

func (o caseFoldStatOps) open(name string) (io.ReadCloser, error) {
	reader, err := o.base.open(name)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return reader, err
	}
	resolved, resolveErr := o.resolveCaseFold(name)
	if resolveErr != nil {
		return nil, err
	}
	return o.base.open(resolved)
}

func (o caseFoldStatOps) stat(name string) (os.FileInfo, error) {
	info, err := o.base.stat(name)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	resolved, resolveErr := o.resolveCaseFold(name)
	if resolveErr != nil {
		return nil, err
	}
	return o.base.stat(resolved)
}

func (o caseFoldStatOps) resolveCaseFold(name string) (string, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	entries, readErr := o.base.readDir(dir)
	if readErr != nil {
		return "", readErr
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), base) {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", os.ErrNotExist
}

func TestCIDMigrationStorage_RequiresExactCIDDirectorySpelling(t *testing.T) {
	recordedCID := storageCID("q")
	content := []byte("case-folded CID must not be selected")

	t.Run("different spelling is absent", func(t *testing.T) {
		root := t.TempDir()
		writeBlob(t, root, strings.ToLower(recordedCID), content)
		a := newCIDMigration(root, caseFoldStatOps{base: osFileOps{}})

		reader, err := a.OpenCIDSource(t.Context(), recordedCID)
		if reader != nil {
			_ = reader.Close()
			t.Fatal("opened a differently cased CID directory entry")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("OpenCIDSource error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("recorded spelling opens", func(t *testing.T) {
		root := t.TempDir()
		writeBlob(t, root, recordedCID, content)
		a := newCIDMigration(root, caseFoldStatOps{base: osFileOps{}})

		reader, err := a.OpenCIDSource(t.Context(), recordedCID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Close() }()
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != string(content) {
			t.Fatalf("ReadAll = (%q, %v)", got, err)
		}
	})
}

func TestCIDMigrationStorage_CaseInsensitiveVariantParksAfterPreparationAndCanonicalizes(t *testing.T) {
	root := t.TempDir()
	cid := storageCID("e")
	content := []byte("case insensitive")
	target := service.ComputeHash(content)
	upper := strings.ToUpper(target)
	secondCID := storageCID("i")
	writeBlob(t, root, cid, content)
	writeBlob(t, root, secondCID, content)
	writeBlob(t, root, upper, content)
	baseOps := osFileOps{}
	a := newCIDMigration(root, caseFoldStatOps{base: baseOps})

	prepared, err := a.PrepareCIDTarget(t.Context(), cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Created || len(prepared.ObsoleteAliases) != 2 {
		t.Fatalf("prepared = %+v", prepared)
	}
	var caseAlias port.CIDStorageAlias
	for _, alias := range prepared.ObsoleteAliases {
		if alias.CanonicalizeCase {
			caseAlias = alias
		}
	}
	if caseAlias.Name != upper {
		t.Fatalf("case alias = %+v", caseAlias)
	}
	if _, err := os.Stat(filepath.Join(root, "_parked", "ipfs-cid-sweep", upper)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alias parked during preparation: %v", err)
	}
	if err := a.FinalizeCIDAlias(t.Context(), caseAlias, target); err != nil {
		t.Fatal(err)
	}
	if got := readBlob(t, filepath.Join(root, target)); string(got) != string(content) {
		t.Fatalf("canonical target = %q", got)
	}
	if got := readBlob(t, filepath.Join(root, "_parked", "ipfs-cid-sweep", upper)); string(got) != string(content) {
		t.Fatalf("parked alias = %q", got)
	}

	second, err := a.PrepareCIDTarget(t.Context(), secondCID, target)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || len(second.ObsoleteAliases) != 1 || second.ObsoleteAliases[0].Name != secondCID {
		t.Fatalf("cached variants were not updated after case canonicalization: %+v", second)
	}
}
