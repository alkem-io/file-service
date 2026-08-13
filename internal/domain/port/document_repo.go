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
	// ListLegacyNamed returns one keyset-paged page of PERMANENT rows whose
	// externalID is not the current content-addressing scheme — i.e. not a
	// lowercase SHA3-256 hex digest (018-legacy-cid-normalization). `after` is the
	// exclusive id cursor; pass uuid.Nil for the first page.
	//
	// Only ID, ExternalID and Version are populated — the identity, the bytes'
	// whereabouts, and the compare-and-set guard. Nothing else is read by the
	// rename, so nothing else is selected. Rows still flagged temporary are
	// excluded: in a cohort this old such a row is orphaned residue, not an
	// upload in flight.
	ListLegacyNamed(ctx context.Context, after uuid.UUID, limit int32) ([]model.Document, error)
	// NormalizeExternalID repoints one row from its legacy name to newExternalID
	// under a compare-and-set on (id, expectedExternalID, expectedVersion), and
	// bumps version. It deliberately leaves mimeType, size, content_metadata and
	// updatedDate alone — the bytes are unchanged, so this is a rename, not an
	// edit.
	//
	// The bool reports whether the write APPLIED. false with a nil error means a
	// concurrent Replace or promotion moved the row between the sweep's read and
	// this write; that writer wins and the sweep must skip the row, never retry.
	NormalizeExternalID(ctx context.Context, id uuid.UUID, expectedExternalID string, expectedVersion int, newExternalID string) (bool, error)
	// FindByExternalIDAndBucket looks up the row matching a content hash
	// within one bucket — the (externalID, storageBucketID) pair that backs
	// per-bucket dedup. Returns model.ErrDocumentNotFound when the bucket
	// holds no row with this content.
	FindByExternalIDAndBucket(ctx context.Context, externalID string, storageBucketID uuid.UUID) (model.Document, error)
	// Create inserts a new document row. contentMetadata is the typed view
	// of the file.content_metadata JSONB column; the adapter owns the
	// serialization to bytes.
	Create(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error)
	// UpdateFile mutates content fields plus content_metadata in one
	// statement (Replace flow). expectedExternalID and expectedVersion are
	// the row identity read before streaming; a mismatch applies zero rows
	// so content replacement is serialized with concurrent replacements and
	// metadata changes. A successful replace increments version.
	UpdateFile(ctx context.Context, id uuid.UUID, expectedExternalID string, expectedVersion int, externalID, mimeType string, size int, contentMetadata model.ContentMetadata) error
	// UpdateMetadata mutates the non-content fields (bucket, temporary
	// location, display name) under optimistic locking: the write applies
	// only while the row's version still equals the given version, and bumps
	// it by one. Returns model.ErrDocumentNotFound on 0 rows affected —
	// which covers both "row gone" and "version mismatch"; the service layer
	// translates that into its concurrency conflict.
	UpdateMetadata(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, displayName string, version int) error
	// BackfillContentMetadata writes computed metadata atomically using
	// compare-and-set: only succeeds if the row currently has empty
	// content_metadata AND its externalID matches expectedExternalID. This
	// protects against a race where Replace ran on the same row between the
	// sweep's storage read and its persist. The bool reports whether the write
	// applied; false with no error means the compare-and-set lost its race.
	BackfillContentMetadata(ctx context.Context, id uuid.UUID, expectedExternalID string, contentMetadata model.ContentMetadata) (bool, error)
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
	// ListImagesNeedingDims returns one keyset-paged page of image rows whose content_metadata is
	// still unpopulated ('{}') — the input to the dims backfill sweep (the sweep-dims job, spec 019/020).
	// afterID is the exclusive cursor (uuid.Nil for the first page); rows are ordered by id, so the
	// last returned doc's ID is the next cursor. Only ID/ExternalID/MimeType are populated (all the
	// sweep needs). An empty page means the sweep has drained the legacy set.
	ListImagesNeedingDims(ctx context.Context, afterID uuid.UUID, limit int32) ([]model.Document, error)
	// UpdateMimeType corrects only the stored MIME type (spec 019 repair-job
	// relabel). Content fields change exclusively via UpdateFile.
	// Compare-and-set: the relabel applies only while the row's externalID
	// still equals expectedExternalID — the content the caller sniffed. A
	// false return (no error) means the guard failed: a concurrent Replace
	// changed the content (and already wrote the correct MIME type) or the
	// row was deleted; the caller must not treat the row as relabeled.
	UpdateMimeType(ctx context.Context, id uuid.UUID, expectedExternalID, mimeType string) (bool, error)
}
