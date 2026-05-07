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
	// Create inserts a new document row. contentMetadata is the raw JSON for
	// the content_metadata column ({"imageWidth":N,"imageHeight":N} for
	// measurable images, {"_decodeFailed":true} for unreadable image bytes,
	// {} for non-image rows or stub-no-decoder).
	Create(ctx context.Context, doc model.Document, contentMetadata []byte) (uuid.UUID, error)
	// UpdateFile mutates content fields plus content_metadata in one
	// statement (Replace flow). The new metadata replaces any prior value.
	UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata []byte) error
	UpdateMetadata(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, displayName string, version int) error
	// BackfillContentMetadata persists computed content_metadata on a row
	// without bumping version (FR-018). Used by the service-layer lazy-
	// backfill helper for legacy rows whose metadata is empty.
	BackfillContentMetadata(ctx context.Context, id uuid.UUID, metadata []byte) error
	Delete(ctx context.Context, id uuid.UUID) (model.DeletedDocument, error)
	CountByExternalID(ctx context.Context, externalID string) (int, error)
}
