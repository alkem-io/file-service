package port

import (
	"context"
	"io"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// StoragePort abstracts the file storage backend (local filesystem, S3, etc.).
type StoragePort interface {
	Save(content []byte) (model.StoredFile, error)
	Read(externalID string) ([]byte, error)
	Delete(externalID string) error
	Exists(externalID string) (bool, error)
	// OpenStage begins a streaming ingestion into not-yet-published storage
	// (spec 020). Nothing is observable as a permanent object until Commit.
	OpenStage(ctx context.Context) (StageWriter, error)
}

// StageWriter is a per-upload staging artifact. It hashes internally while
// bytes are written; Commit finalizes the content identity and performs the
// backend-specific publish (FS: dedup-stat + rename; an object store would
// complete a multipart upload and copy to the content-addressed key — the
// contract deliberately assumes no atomic rename primitive).
type StageWriter interface {
	io.Writer
	// Commit publishes the staged content under its content hash and
	// returns the stored identity. Created=false signals a dedup hit (the
	// blob already existed); the staging artifact is destroyed either way.
	Commit() (model.StoredFile, error)
	// Abort destroys the staging artifact. Idempotent; safe after a failed
	// Commit and after a successful one (no-op then).
	Abort() error
}
