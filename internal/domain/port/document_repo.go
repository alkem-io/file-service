package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// DocumentRepo abstracts database access to the document table.
type DocumentRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (model.Document, error)
	FindByExternalIDAndBucket(ctx context.Context, externalID string, storageBucketID uuid.UUID) (model.Document, error)
	// Create inserts a new document row. contentMetadata is the typed view
	// of the file.content_metadata JSONB column; the adapter owns the
	// serialization to bytes.
	Create(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error)
	// UpdateFile mutates content fields plus content_metadata in one
	// statement (Replace flow). The new metadata replaces any prior value.
	UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata) error
	UpdateMetadata(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, displayName string, version int) error
	// BackfillContentMetadata writes computed metadata atomically using
	// compare-and-set: only succeeds if the row currently has empty
	// content_metadata AND its externalID matches expectedExternalID. This
	// protects against a race where Replace ran on the same row between the
	// lazy-backfill's storage read and its persist. A 0-rows-affected outcome
	// signals "lost the race" — the helper returns nil (treats as success;
	// the winner already wrote fresh metadata).
	BackfillContentMetadata(ctx context.Context, id uuid.UUID, expectedExternalID string, contentMetadata model.ContentMetadata) error
	Delete(ctx context.Context, id uuid.UUID) (model.DeletedDocument, error)
	CountByExternalID(ctx context.Context, externalID string) (int, error)
	// ListByMimeTypes returns documents whose stored MIME type is one of the
	// given values (spec 019 repair-job scan). Only ID, ExternalID, MimeType,
	// Size and DisplayName are populated.
	ListByMimeTypes(ctx context.Context, mimeTypes []string) ([]model.Document, error)
	// UpdateMimeType corrects only the stored MIME type (spec 019 repair-job
	// relabel). Content fields change exclusively via UpdateFile.
	UpdateMimeType(ctx context.Context, id uuid.UUID, mimeType string) error
}
