package alkemiodb

import (
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/alkem-io/file-service-go/internal/adapter/outbound/alkemiodb/queries"
	"github.com/alkem-io/file-service-go/internal/domain/model"
)

func uuidToPgx(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func uuidToPgxNullable(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{Valid: false}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

func pgxToUUID(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil
	}
	return id.Bytes
}

func pgxToUUIDNullable(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	u := uuid.UUID(id.Bytes)
	return &u
}

func timeToPgx(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timeToPgxNow() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

func pgxToTime(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}

func rowToDocument(row queries.GetDocumentByIDRow) model.Document {
	return model.Document{
		ID:                pgxToUUID(row.ID),
		ExternalID:        row.ExternalID,
		MimeType:          row.MimeType,
		Size:              int(row.Size),
		DisplayName:       row.DisplayName,
		CreatedBy:         pgxToUUIDNullable(row.CreatedBy),
		TemporaryLocation: row.TemporaryLocation,
		StorageBucketID:   pgxToUUID(row.StorageBucketId),
		AuthorizationID:   pgxToUUID(row.AuthorizationId),
		TagsetID:          pgxToUUIDNullable(row.TagsetId),
		CreatedDate:       pgxToTime(row.CreatedDate),
		UpdatedDate:       pgxToTime(row.UpdatedDate),
		Version:           int(row.Version),
	}
}

func findRowToDocument(row queries.FindDocumentByExternalIDAndBucketRow) model.Document {
	return model.Document{
		ID:                pgxToUUID(row.ID),
		ExternalID:        row.ExternalID,
		MimeType:          row.MimeType,
		Size:              int(row.Size),
		DisplayName:       row.DisplayName,
		CreatedBy:         pgxToUUIDNullable(row.CreatedBy),
		TemporaryLocation: row.TemporaryLocation,
		StorageBucketID:   pgxToUUID(row.StorageBucketId),
		AuthorizationID:   pgxToUUID(row.AuthorizationId),
		TagsetID:          pgxToUUIDNullable(row.TagsetId),
		CreatedDate:       pgxToTime(row.CreatedDate),
		UpdatedDate:       pgxToTime(row.UpdatedDate),
		Version:           int(row.Version),
	}
}
