package port

import (
	"context"
	"io"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// StoragePort abstracts the file storage backend (local filesystem, S3, etc.).
// Storage is content-addressed: externalID is the hash of the blob's bytes.
type StoragePort interface {
	// Save stores a complete in-memory buffer and returns its content
	// identity. Saving bytes that already exist is not an error: the
	// existing blob is reused and StoredFile.Created reports false.
	Save(content []byte) (model.StoredFile, error)
	// Read returns the whole blob. Wraps the backend's not-found error
	// (os.ErrNotExist for the local adapter) when no blob has this id.
	Read(externalID string) ([]byte, error)
	// Delete removes the blob. Idempotent: deleting an id that does not
	// exist returns nil, so refcount-driven cleanup can race harmlessly.
	Delete(externalID string) error
	// Exists reports whether a blob is present without reading its bytes.
	// (false, nil) is a definitive "not there"; a non-nil error means the
	// backend could not answer.
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
