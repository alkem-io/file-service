package port_test

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// StoragePortContractTest exercises any StoragePort implementation. One subtest per port method,
// so its branch count is inherent contract coverage, not complexity to refactor.
//
//nolint:gocyclo // per-method subtests; splitting fragments the single-entry contract, not worth it
func StoragePortContractTest(t *testing.T, storage port.StoragePort) {
	t.Helper()

	t.Run("Save and Read", func(t *testing.T) {
		content := []byte("contract test content")
		stored, err := storage.Save(content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if stored.ExternalID == "" {
			t.Fatal("empty externalID")
		}
		if stored.Size != len(content) {
			t.Errorf("size = %d, want %d", stored.Size, len(content))
		}

		data, err := storage.Read(stored.ExternalID)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if string(data) != string(content) {
			t.Error("content mismatch")
		}
	})

	t.Run("ReadStream", func(t *testing.T) {
		content := []byte("stream contract content")
		stored, err := storage.Save(content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		rc, size, err := storage.ReadStream(stored.ExternalID)
		if err != nil {
			t.Fatalf("ReadStream: %v", err)
		}
		defer func() { _ = rc.Close() }()
		if size != int64(len(content)) {
			t.Errorf("ReadStream size = %d, want %d", size, len(content))
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("ReadStream body: %v", err)
		}
		if string(got) != string(content) {
			t.Errorf("ReadStream content = %q, want %q", got, content)
		}

		// The error contract the by-hash handler's 400/404 mapping depends on:
		// absent blob → os.ErrNotExist; malformed key → ErrInvalidKey.
		absent := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if _, _, err := storage.ReadStream(absent); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadStream absent blob: err = %v, want os.ErrNotExist", err)
		}
		traversal := strings.Repeat("../", 20) + "etc/passwd"
		if _, _, err := storage.ReadStream(traversal); !errors.Is(err, port.ErrInvalidKey) {
			t.Errorf("ReadStream malformed key: err = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("Content Addressable", func(t *testing.T) {
		content := []byte("same content for dedup")
		s1, err := storage.Save(content)
		if err != nil {
			t.Fatalf("first Save: %v", err)
		}
		s2, err := storage.Save(content)
		if err != nil {
			t.Fatalf("second Save: %v", err)
		}
		if s1.ExternalID != s2.ExternalID {
			t.Errorf("not content-addressable: %q != %q", s1.ExternalID, s2.ExternalID)
		}
	})

	t.Run("Idempotent Writes", func(t *testing.T) {
		content := []byte("idempotent")
		_, err1 := storage.Save(content)
		_, err2 := storage.Save(content)
		if err1 != nil || err2 != nil {
			t.Errorf("idempotent save failed: %v, %v", err1, err2)
		}
	})

	t.Run("Exists", func(t *testing.T) {
		content := []byte("exists test")
		stored, err := storage.Save(content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}

		exists, err := storage.Exists(stored.ExternalID)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Error("expected file to exist")
		}

		absent := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		exists, err = storage.Exists(absent)
		if err != nil {
			t.Fatalf("Exists absent valid key: %v", err)
		}
		if exists {
			t.Error("expected file to not exist")
		}
	})

	t.Run("Delete and Exists", func(t *testing.T) {
		content := []byte("delete me in contract")
		stored, err := storage.Save(content)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}

		err = storage.Delete(stored.ExternalID)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}

		exists, err := storage.Exists(stored.ExternalID)
		if err != nil {
			t.Fatalf("Exists after Delete: %v", err)
		}
		if exists {
			t.Error("file still exists after delete")
		}
	})

	t.Run("Read Missing", func(t *testing.T) {
		absent := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		_, err := storage.Read(absent)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Read missing: err = %v, want os.ErrNotExist", err)
		}
	})
}

func TestLocalAdapter_StoragePortContract(t *testing.T) {
	adapter := local.New(t.TempDir())
	StoragePortContractTest(t, adapter)
}
