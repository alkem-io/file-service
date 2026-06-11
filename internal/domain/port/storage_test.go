package port_test

import (
	"testing"

	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// StoragePortContractTest exercises any StoragePort implementation.
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

	t.Run("Content Addressable", func(t *testing.T) {
		content := []byte("same content for dedup")
		s1, _ := storage.Save(content)
		s2, _ := storage.Save(content)
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
		stored, _ := storage.Save(content)

		exists, err := storage.Exists(stored.ExternalID)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Error("expected file to exist")
		}

		exists, _ = storage.Exists("nonexistent-hash")
		if exists {
			t.Error("expected file to not exist")
		}
	})

	t.Run("Delete and Exists", func(t *testing.T) {
		content := []byte("delete me in contract")
		stored, _ := storage.Save(content)

		err := storage.Delete(stored.ExternalID)
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}

		exists, _ := storage.Exists(stored.ExternalID)
		if exists {
			t.Error("file still exists after delete")
		}
	})

	t.Run("Read Missing", func(t *testing.T) {
		_, err := storage.Read("definitely-not-here")
		if err == nil {
			t.Error("expected error for missing file")
		}
	})
}

func TestLocalAdapter_StoragePortContract(t *testing.T) {
	adapter := local.New(t.TempDir())
	StoragePortContractTest(t, adapter)
}
