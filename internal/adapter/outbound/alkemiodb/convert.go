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

// documentRow is the subset of fields shared by every sqlc query row that
// materializes a full document record. sqlc emits a distinct Go type per
// query; Go structural conversion lets us map any row type into this one
// as long as the fields match exactly. Field names must match sqlc's
// generated casing (StorageBucketId, not StorageBucketID) — hence the
// nolint directives below.
type documentRow struct {
	ID                pgtype.UUID
	ExternalID        string
	MimeType          string
	Size              int32
	DisplayName       string
	CreatedBy         pgtype.UUID
	TemporaryLocation bool
	StorageBucketId   pgtype.UUID //nolint:staticcheck // ST1003: sqlc-generated casing must match for structural conversion
	AuthorizationId   pgtype.UUID //nolint:staticcheck // ST1003: sqlc-generated casing must match for structural conversion
	TagsetId          pgtype.UUID //nolint:staticcheck // ST1003: sqlc-generated casing must match for structural conversion
	CreatedDate       pgtype.Timestamptz
	UpdatedDate       pgtype.Timestamptz
	Version           int32
}

func documentFromRow(r documentRow) model.Document {
	return model.Document{
		ID:                pgxToUUID(r.ID),
		ExternalID:        r.ExternalID,
		MimeType:          r.MimeType,
		Size:              int(r.Size),
		DisplayName:       r.DisplayName,
		CreatedBy:         pgxToUUIDNullable(r.CreatedBy),
		TemporaryLocation: r.TemporaryLocation,
		StorageBucketID:   pgxToUUID(r.StorageBucketId),
		AuthorizationID:   pgxToUUID(r.AuthorizationId),
		TagsetID:          pgxToUUIDNullable(r.TagsetId),
		CreatedDate:       pgxToTime(r.CreatedDate),
		UpdatedDate:       pgxToTime(r.UpdatedDate),
		Version:           int(r.Version),
	}
}

func rowToDocument(row queries.GetDocumentByIDRow) model.Document {
	return documentFromRow(documentRow(row))
}

func findRowToDocument(row queries.FindDocumentByExternalIDAndBucketRow) model.Document {
	return documentFromRow(documentRow(row))
}
