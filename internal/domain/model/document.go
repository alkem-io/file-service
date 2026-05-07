package model

import (
	"time"

	"github.com/google/uuid"
)

// Document represents a full document record from Alkemio's document table.
type Document struct {
	ID                uuid.UUID
	ExternalID        string
	MimeType          string
	Size              int
	DisplayName       string
	CreatedBy         *uuid.UUID
	TemporaryLocation bool
	StorageBucketID   uuid.UUID
	AuthorizationID   uuid.UUID
	TagsetID          *uuid.UUID
	CreatedDate       time.Time
	UpdatedDate       time.Time
	Version           int

	// Reused is set to true when a dedup lookup returned an existing row
	// instead of inserting a new one. Response-only; not persisted.
	// When Reused is true, the caller-supplied AuthorizationID and TagsetID
	// were ignored — the existing row's values are authoritative.
	Reused bool

	// ImageWidth and ImageHeight are post-rotation pixel dimensions for
	// raster images, SVG (from viewBox), and GIF (canvas dims). Both nil
	// when the upload is not an image, or when the row's content_metadata
	// is empty/sentinel. Sourced from the JSONB content_metadata column on
	// read; populated from ProcessResult / lazy-backfill on write paths.
	ImageWidth  *int
	ImageHeight *int
}

// CreateDocumentInput contains fields needed to create a new document.
type CreateDocumentInput struct {
	DisplayName       string
	CreatedBy         *uuid.UUID
	TemporaryLocation bool
	StorageBucketID   uuid.UUID
	AuthorizationID   uuid.UUID
	TagsetID          *uuid.UUID

	// SkipDedup, when true, bypasses the per-bucket content-hash dedup
	// lookup and forces a fresh row insert even if an existing row in the
	// same bucket has the same externalID. Use case: placeholder uploads
	// (e.g., empty buffers for Collabora documents) where two logical
	// documents must NOT share a backing row even though their content
	// hashes match. Default false preserves the dedup behavior.
	SkipDedup bool
}

// CopyDocumentInput contains fields needed to copy an existing document into
// another bucket. The new row references the same content (same externalID,
// mimeType, size, displayName) as the source; only ownership/placement
// changes. The source row is not modified.
type CopyDocumentInput struct {
	DestinationBucketID uuid.UUID
	AuthorizationID     uuid.UUID
	TagsetID            *uuid.UUID
	CreatedBy           *uuid.UUID

	// SkipDedup mirrors the same flag on CreateDocumentInput. Default false
	// runs the per-bucket dedup lookup; true forces a fresh row insert.
	SkipDedup bool
}

// StoredFile represents the result of a file storage operation.
type StoredFile struct {
	ExternalID string
	MimeType   string
	Size       int
	Created    bool // true if a new file was written; false if dedup matched an existing file

	// ImageWidth and ImageHeight are post-rotation pixel dimensions for
	// the just-stored bytes, populated by StoreAndLink from the result of
	// Process. Nil for non-image content. Used by the Replace handler to
	// surface dims on ReplaceContentResponse without re-reading the row.
	ImageWidth  *int
	ImageHeight *int
}

// AuthResult represents the outcome of an authorization check.
type AuthResult struct {
	Allowed bool
	Reason  string
}

// DeletedDocument contains IDs the caller needs for cleanup after deletion.
type DeletedDocument struct {
	ExternalID      string
	AuthorizationID uuid.UUID
	TagsetID        *uuid.UUID
}
