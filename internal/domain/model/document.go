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
}

// CreateDocumentInput contains fields needed to create a new document.
type CreateDocumentInput struct {
	DisplayName       string
	CreatedBy         *uuid.UUID
	TemporaryLocation bool
	StorageBucketID   uuid.UUID
	AuthorizationID   uuid.UUID
	TagsetID          *uuid.UUID
}

// StoredFile represents the result of a file storage operation.
type StoredFile struct {
	ExternalID string
	MimeType   string
	Size       int
	Created    bool // true if a new file was written; false if dedup matched an existing file
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
