package port

import (
	"context"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// DocumentRepo abstracts database access to the document table.
type DocumentRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (model.Document, error)
	Create(ctx context.Context, doc model.Document) (uuid.UUID, error)
	UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int) error
	UpdateLocation(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool) error
	Delete(ctx context.Context, id uuid.UUID) (model.DeletedDocument, error)
	CountByExternalID(ctx context.Context, externalID string) (int, error)
}
