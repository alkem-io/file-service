package local

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/service"
)

// Adapter implements port.StoragePort using the local filesystem.
type Adapter struct {
	basePath string
}

func New(basePath string) *Adapter {
	return &Adapter{basePath: basePath}
}

func (a *Adapter) Save(content []byte) (model.StoredFile, error) {
	externalID := service.ComputeHash(content)
	filePath := a.filePath(externalID)

	// Content-addressable dedup: skip if file already exists
	if _, err := os.Stat(filePath); err == nil {
		return model.StoredFile{
			ExternalID: externalID,
			Size:       len(content),
		}, nil
	}

	// Atomic write: temp file + rename
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return model.StoredFile{}, fmt.Errorf("create storage dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return model.StoredFile{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return model.StoredFile{}, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return model.StoredFile{}, fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, filePath); err != nil {
		_ = os.Remove(tmpName)
		return model.StoredFile{}, fmt.Errorf("rename to final path: %w", err)
	}

	return model.StoredFile{
		ExternalID: externalID,
		Size:       len(content),
	}, nil
}

func (a *Adapter) Read(externalID string) ([]byte, error) {
	if !isValidExternalID(externalID) {
		return nil, fmt.Errorf("invalid external ID: %s", externalID)
	}
	data, err := os.ReadFile(a.filePath(externalID))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", externalID, err)
	}
	return data, nil
}

func (a *Adapter) Delete(externalID string) error {
	if !isValidExternalID(externalID) {
		return fmt.Errorf("invalid external ID: %s", externalID)
	}
	err := os.Remove(a.filePath(externalID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", externalID, err)
	}
	return nil
}

func (a *Adapter) Exists(externalID string) (bool, error) {
	if !isValidExternalID(externalID) {
		return false, fmt.Errorf("invalid external ID: %s", externalID)
	}
	_, err := os.Stat(a.filePath(externalID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (a *Adapter) filePath(externalID string) string {
	return filepath.Join(a.basePath, externalID)
}

// isValidExternalID checks that the ID is a valid SHA3-256 hex string (64 lowercase hex chars).
func isValidExternalID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
