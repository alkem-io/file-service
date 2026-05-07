package alkemiodb

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

func (a *Adapter) FindByExternalIDAndBucket(ctx context.Context, externalID string, storageBucketID uuid.UUID) (model.Document, error) {
	row, err := a.queries.FindDocumentByExternalIDAndBucket(ctx, queries.FindDocumentByExternalIDAndBucketParams{
		ExternalID:      externalID,
		StorageBucketId: uuidToPgx(storageBucketID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Document{}, model.ErrDocumentNotFound
		}
		return model.Document{}, err
	}
	return findRowToDocument(row), nil
}

func (a *Adapter) Create(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error) {
	raw, err := marshalContentMetadata(contentMetadata)
	if err != nil {
		return uuid.Nil, err
	}
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
		ContentMetadata:   raw,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return uuid.Nil, model.ErrDuplicateKey
		}
		return uuid.Nil, err
	}
	return pgxToUUID(id), nil
}

func (a *Adapter) UpdateFile(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata) error {
	raw, err := marshalContentMetadata(contentMetadata)
	if err != nil {
		return err
	}
	rows, err := a.queries.UpdateDocumentFile(ctx, queries.UpdateDocumentFileParams{
		ID:              uuidToPgx(id),
		ExternalID:      externalID,
		MimeType:        mimeType,
		Size:            safeInt32(size),
		UpdatedDate:     timeToPgxNow(),
		ContentMetadata: raw,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return model.ErrDuplicateKey
		}
		return err
	}
	if rows == 0 {
		return model.ErrDocumentNotFound
	}
	return nil
}

func (a *Adapter) UpdateMetadata(ctx context.Context, id uuid.UUID, storageBucketID uuid.UUID, temporaryLocation bool, displayName string, version int) error {
	rows, err := a.queries.UpdateDocumentMetadata(ctx, queries.UpdateDocumentMetadataParams{
		ID:                uuidToPgx(id),
		StorageBucketId:   uuidToPgx(storageBucketID),
		TemporaryLocation: temporaryLocation,
		DisplayName:       displayName,
		UpdatedDate:       timeToPgxNow(),
		Version:           safeInt32(version),
	})
	if err != nil {
		// Defensive: keeps PATCH consistent with Create/UpdateFile if a
		// uniqueness constraint is added later (e.g. (externalID,
		// storageBucketId)). No such constraint exists in this repo's
		// db/schema/document.sql today, but the production schema can
		// diverge, and a 409 beats a 500 if it fires.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return model.ErrDuplicateKey
		}
		return err
	}
	if rows == 0 {
		return model.ErrDocumentNotFound
	}
	return nil
}

// BackfillContentMetadata persists computed content_metadata on a row without
// bumping version. Compare-and-set: only writes when content_metadata is still
// empty AND the row's externalID matches expectedExternalID. A 0-rows-affected
// outcome means another writer (Replace, or another backfill) won the race; we
// treat that as a non-error since the winner already wrote fresh metadata.
// FR-018.
func (a *Adapter) BackfillContentMetadata(ctx context.Context, id uuid.UUID, expectedExternalID string, contentMetadata model.ContentMetadata) error {
	raw, err := marshalContentMetadata(contentMetadata)
	if err != nil {
		return err
	}
	_, err = a.queries.BackfillContentMetadata(ctx, queries.BackfillContentMetadataParams{
		ID:              uuidToPgx(id),
		ContentMetadata: raw,
		ExternalID:      expectedExternalID,
	})
	// rowsAffected==0 → race lost (treated as success); any DB error
	// surfaces normally.
	return err
}

// marshalContentMetadata serializes the typed value to the JSONB shape stored
// in the file.content_metadata column. Empty Populated → "{}". DecodeFailed
// → {"_decodeFailed": true}. Both dims set → {"imageWidth": N, "imageHeight":
// M}. Other shapes (e.g. non-image rows where Populated=true) default to "{}".
func marshalContentMetadata(meta model.ContentMetadata) ([]byte, error) {
	if !meta.Populated {
		return []byte(`{}`), nil
	}
	if meta.DecodeFailed {
		return []byte(`{"_decodeFailed":true}`), nil
	}
	if meta.ImageWidth != nil && meta.ImageHeight != nil {
		return json.Marshal(struct {
			ImageWidth  int `json:"imageWidth"`
			ImageHeight int `json:"imageHeight"`
		}{*meta.ImageWidth, *meta.ImageHeight})
	}
	return []byte(`{}`), nil
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
