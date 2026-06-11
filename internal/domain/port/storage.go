package port

import "github.com/alkem-io/file-service/internal/domain/model"

// StoragePort abstracts the file storage backend (local filesystem, S3, etc.).
type StoragePort interface {
	Save(content []byte) (model.StoredFile, error)
	Read(externalID string) ([]byte, error)
	Delete(externalID string) error
	Exists(externalID string) (bool, error)
}
