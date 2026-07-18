package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// DocumentRepo abstracts database access to the document table.
type DocumentRepo interface {
	// GetByID fetches the full document row. Returns
	// model.ErrDocumentNotFound when no row has this id.
	GetByID(ctx context.Context, id uuid.UUID) (model.Document, error)
	// FindByExternalIDAndBucket looks up the row matching a content hash
	// within one bucket — the (externalID, storageBucketID) pair that backs
	// per-bucket dedup. Returns model.ErrDocumentNotFound when the bucket
	// holds no row with this content.
	FindByExternalIDAndBucket(ctx context.Context, externalID string, storageBucketID uuid.UUID) (model.Document, error)
	// GetByReference resolves the opaque externalReference across ALL buckets
	// (the provider's global fetch). Several buckets may carry the same
	// reference (re-share, one shared blob); the oldest row wins. Returns
	// model.ErrDocumentNotFound when no row carries the reference.
	GetByReference(ctx context.Context, reference string) (model.Document, error)
	// GetByReferenceInBucket resolves the opaque externalReference within one
	// bucket (read resolution). UNIQUE(externalReference, storageBucketID)
	// guarantees at most one match. Returns model.ErrDocumentNotFound when the
	// bucket holds no row with this reference.
	GetByReferenceInBucket(ctx context.Context, reference string, storageBucketID uuid.UUID) (model.Document, error)
	// Create inserts a new document row. contentMetadata is the typed view
	// of the file.content_metadata JSONB column; the adapter owns the
	// serialization to bytes.
	Create(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error)
	// UpdateFile mutates content fields plus content_metadata in one
	// statement (Replace flow). The new metadata replaces any prior value.
	UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata) error
	// UpdateMetadata mutates the non-content fields (bucket, temporary
	// location, display name) under optimistic locking: the write applies
	// only while the row's version still equals the given version, and bumps
	// it by one. Returns model.ErrDocumentNotFound on 0 rows affected —
	// which covers both "row gone" and "version mismatch"; the service layer
	// translates that into its concurrency conflict.
	//
	// It returns the AUTHORITATIVE post-update externalID and size — read from
	// the row while it is UPDATE-locked in the same statement (RETURNING) — so
	// the caller can build the response and (on the transactional path) enqueue
	// the backup hint from the real durable content, never a stale
	// handler-threaded snapshot that a concurrent content-replace could have
	// invalidated. Both are zero-valued when the update matched no row.
	//
	// This is the "move + re-attribute" primitive: besides bucket/temporary-
	// location/display-name it also re-points authorizationID, createdBy, and
	// the opaque externalReference. The caller supplies every field's intended
	// final value (the handler fills omitted fields from the current row).
	UpdateMetadata(ctx context.Context, id uuid.UUID, meta model.DocumentMetadataUpdate, version int) (externalID string, size int, err error)
	// BackfillContentMetadata writes computed metadata atomically using
	// compare-and-set: only succeeds if the row currently has empty
	// content_metadata AND its externalID matches expectedExternalID. This
	// protects against a race where Replace ran on the same row between the
	// lazy-backfill's storage read and its persist. A 0-rows-affected outcome
	// signals "lost the race" — the helper returns nil (treats as success;
	// the winner already wrote fresh metadata).
	BackfillContentMetadata(ctx context.Context, id uuid.UUID, expectedExternalID string, contentMetadata model.ContentMetadata) error
	// Delete removes the row and returns the identifiers the caller needs
	// for cleanup: the content hash (to decide whether the blob is now
	// orphaned) and the authorization/tagset ids (owned by alkemio-server,
	// which deletes them). Returns model.ErrDocumentNotFound if the row
	// does not exist — deletes are not idempotent at this layer.
	Delete(ctx context.Context, id uuid.UUID) (model.DeletedDocument, error)
	// CountByExternalID reports how many rows (across all buckets) still
	// reference a content hash — the blob refcount that gates physical
	// deletion from storage.
	CountByExternalID(ctx context.Context, externalID string) (int, error)
	// ListByMimeTypes returns documents whose stored MIME type is one of the
	// given values (spec 019 repair-job scan). Only ID, ExternalID, MimeType,
	// Size and DisplayName are populated.
	ListByMimeTypes(ctx context.Context, mimeTypes []string) ([]model.Document, error)
	// UpdateMimeType corrects only the stored MIME type (spec 019 repair-job
	// relabel). Content fields change exclusively via UpdateFile.
	// Compare-and-set: the relabel applies only while the row's externalID
	// still equals expectedExternalID — the content the caller sniffed. A
	// false return (no error) means the guard failed: a concurrent Replace
	// changed the content (and already wrote the correct MIME type) or the
	// row was deleted; the caller must not treat the row as relabeled.
	UpdateMimeType(ctx context.Context, id uuid.UUID, expectedExternalID, mimeType string) (bool, error)
}
