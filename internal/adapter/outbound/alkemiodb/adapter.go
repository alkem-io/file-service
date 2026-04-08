package alkemiodb

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/alkem-io/file-service-go/internal/adapter/outbound/alkemiodb/queries"
	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// Adapter implements port.DocumentRepo using pgx/sqlc.
type Adapter struct {
	queries *queries.Queries
}

// New creates an Adapter from any DBTX-compatible connection (pgxpool.Pool, pgxmock, etc.).
func New(db queries.DBTX) *Adapter {
	return &Adapter{
		queries: queries.New(db),
	}
}

func (a *Adapter) GetByID(ctx context.Context, id uuid.UUID) (model.Document, error) {
	row, err := a.queries.GetDocumentByID(ctx, uuidToPgx(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Document{}, model.ErrDocumentNotFound
		}
		return model.Document{}, err
	}
	return rowToDocument(row), nil
}

func (a *Adapter) Create(ctx context.Context, doc model.Document) (uuid.UUID, error) {
	id, err := a.queries.CreateDocument(ctx, queries.CreateDocumentParams{
		ID:                uuidToPgx(doc.ID),
		ExternalID:        doc.ExternalID,
		MimeType:          doc.MimeType,
		Size:              safeInt32(doc.Size),
		DisplayName:       doc.DisplayName,
		CreatedBy:         uuidToPgxNullable(doc.CreatedBy),
		TemporaryLocation: doc.TemporaryLocation,
		StorageBucketId:   uuidToPgx(doc.StorageBucketID),
		AuthorizationId:   uuidToPgx(doc.AuthorizationID),
		TagsetId:          uuidToPgxNullable(doc.TagsetID),
		CreatedDate:       timeToPgx(doc.CreatedDate),
		UpdatedDate:       timeToPgx(doc.UpdatedDate),
	})
	if err != nil {
		return uuid.Nil, err
	}
	return pgxToUUID(id), nil
}

func (a *Adapter) UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int) error {
	rows, err := a.queries.UpdateDocumentFile(ctx, queries.UpdateDocumentFileParams{
		ID:          uuidToPgx(id),
		ExternalID:  externalID,
		MimeType:    mimeType,
		Size:        safeInt32(size),
		UpdatedDate: timeToPgxNow(),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return model.ErrDocumentNotFound
	}
	return nil
}

func (a *Adapter) UpdateLocation(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, version int) error {
	rows, err := a.queries.UpdateDocumentLocation(ctx, queries.UpdateDocumentLocationParams{
		ID:                uuidToPgx(id),
		StorageBucketId:   uuidToPgx(storageBucketID),
		TemporaryLocation: temporaryLocation,
		UpdatedDate:       timeToPgxNow(),
		Version:           safeInt32(version),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return model.ErrDocumentNotFound
	}
	return nil
}

func (a *Adapter) Delete(ctx context.Context, id uuid.UUID) (model.DeletedDocument, error) {
	row, err := a.queries.DeleteDocument(ctx, uuidToPgx(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.DeletedDocument{}, model.ErrDocumentNotFound
		}
		return model.DeletedDocument{}, err
	}
	return model.DeletedDocument{
		ExternalID:      row.ExternalID,
		AuthorizationID: pgxToUUID(row.AuthorizationId),
		TagsetID:        pgxToUUIDNullable(row.TagsetId),
	}, nil
}

func (a *Adapter) CountByExternalID(ctx context.Context, externalID string) (int, error) {
	count, err := a.queries.CountDocumentsByExternalID(ctx, externalID)
	if err != nil {
		return 0, err
	}
	return int(count), nil
}
