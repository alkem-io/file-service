package port

import (
	"context"
	"errors"
	"io"

	"github.com/alkem-io/file-service/internal/domain/model"
)

var (
	// ErrInvalidKey is returned when an externalID fails the storage key rules
	// (path-traversal guard / length bound). Distinct from a genuinely absent
	// blob, so a handler can answer 400 rather than 404/500.
	ErrInvalidKey = errors.New("invalid storage key")
	// ErrStoreUnavailable is returned when the storage backend itself is
	// unreachable — e.g. the local volume is unmounted or its root directory is
	// gone, so every read would surface as not-found. Callers MUST treat this as
	// a RETRYABLE outage, never as an authoritative "object deleted": a
	// backup/replication reader that conflates a mount outage with a missing
	// object would record still-existing objects as gone across the whole store.
	ErrStoreUnavailable = errors.New("storage backend unavailable")
)

// StoragePort abstracts the file storage backend (local filesystem, S3, etc.).
// Storage is content-addressed: externalID is the hash of the blob's bytes.
type StoragePort interface {
	// Save stores a complete in-memory buffer and returns its content
	// identity. Saving bytes that already exist is not an error: the
	// existing blob is reused and StoredFile.Created reports false.
	Save(content []byte) (model.StoredFile, error)
	// Read returns the whole blob. Error contract: ErrInvalidKey (malformed id),
	// os.ErrNotExist (blob genuinely absent, store healthy), ErrStoreUnavailable
	// (backend outage — retryable, NOT an authoritative absence).
	Read(externalID string) ([]byte, error)
	// ReadStream opens the blob for streaming (constant memory — the caller does
	// not hold the whole blob resident), returning the content, its size, and a
	// closer the caller MUST close. Same error contract as Read. Prefer this for
	// bulk/high-concurrency readers so N concurrent fetches don't cost N×blobsize.
	ReadStream(externalID string) (io.ReadCloser, int64, error)
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
	// StagedReaderAt exposes the bytes written so far for random-access
	// inspection before Commit (e.g. reading a zip central directory). Valid
	// only after writes complete and before Commit/Abort. The local FS backend
	// returns the staging temp file; an object-store backend would satisfy this
	// with ranged reads of the staged object (zip.NewReader uses ReadAt on the
	// tail, so ranged GETs suffice).
	StagedReaderAt() (io.ReaderAt, int64, error)
}
