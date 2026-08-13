package http

import (
	"encoding/json"
	"net/http"
	"time"
)

// CreateDocumentResponse is returned by POST /internal/file.
// Always uses HTTP 201 so strict POST clients and code generators treat
// every success uniformly. The Reused field distinguishes outcomes:
//   - Reused=false: a new file row was inserted
//   - Reused=true:  an existing row matched (externalID, storageBucketID)
//     and was returned as-is; the caller-supplied authorizationId/tagsetId
//     were ignored and should be cleaned up by the caller.
type CreateDocumentResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalID"`
	MimeType   string `json:"mimeType"`
	Size       int    `json:"size"`
	Reused     bool   `json:"reused"`
	// ImageWidth/ImageHeight are post-rotation pixel dimensions sourced from
	// content_metadata for image rows. Both nil for non-images and for
	// image rows whose metadata is empty/sentinel.
	ImageWidth  *int `json:"imageWidth,omitempty"`
	ImageHeight *int `json:"imageHeight,omitempty"`
}

// Render writes the response as JSON with HTTP 201 (always — see the type
// comment for why dedup reuse is not a 200).
func (r CreateDocumentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(r)
}

// DeleteDocumentResponse is returned by DELETE /internal/document/:id.
//
// AuthorizationID is OPTIONAL on the wire: a document stored without a
// server-minted authorization (the matrix_media staging store) has a NULL
// authorizationId column, which reads back as the zero UUID. Serializing that
// as "00000000-0000-0000-0000-000000000000" would tell the caller to clean up
// a policy that never existed, so the field is omitted instead.
type DeleteDocumentResponse struct {
	AuthorizationID *string `json:"authorizationId,omitempty"`
	TagsetID        *string `json:"tagsetId,omitempty"`
}

// Render writes the response as JSON with HTTP 200.
func (r DeleteDocumentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// UpdateDocumentResponse is returned by PATCH /internal/file/:id.
type UpdateDocumentResponse struct {
	ID                string `json:"id"`
	StorageBucketID   string `json:"storageBucketId"`
	TemporaryLocation bool   `json:"temporaryLocation"`
	DisplayName       string `json:"displayName"`
	// ImageWidth/ImageHeight are post-rotation pixel dimensions sourced from
	// content_metadata. Both nil for non-image rows and for image rows
	// whose metadata is empty/sentinel.
	ImageWidth  *int `json:"imageWidth,omitempty"`
	ImageHeight *int `json:"imageHeight,omitempty"`
}

// Render writes the response as JSON with HTTP 200.
func (r UpdateDocumentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// ReplaceContentResponse is returned by PUT /internal/document/:id/content.
type ReplaceContentResponse struct {
	ExternalID string `json:"externalID"`
	MimeType   string `json:"mimeType"`
	Size       int    `json:"size"`
	// ImageWidth/ImageHeight are post-rotation pixel dimensions for the
	// just-replaced bytes, populated from the ProcessResult inside
	// StoreAndLink. Both nil for non-image content.
	ImageWidth  *int `json:"imageWidth,omitempty"`
	ImageHeight *int `json:"imageHeight,omitempty"`
}

// Render writes the response as JSON with HTTP 200.
func (r ReplaceContentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// MimeMismatchDetail carries the MIME pair behind a MIME_MISMATCH rejection.
type MimeMismatchDetail struct {
	KnownMime    string `json:"knownMime"`
	DetectedMime string `json:"detectedMime"`
}

// RejectedContentResponse is the 422 body for content-replace rejections
// (spec 019). Code is machine-readable and stable: EMPTY_CONTENT or
// MIME_MISMATCH. Detail is present only for MIME_MISMATCH.
type RejectedContentResponse struct {
	Code   string              `json:"code"`
	Error  string              `json:"error"`
	Detail *MimeMismatchDetail `json:"detail,omitempty"`
}

// Render writes the rejection as JSON with HTTP 422 Unprocessable Entity —
// the request was well-formed; the content itself was refused.
func (r RejectedContentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(r)
}

// DocumentMetaResponse is returned by GET /internal/document/:id/meta and by
// GET /internal/file/by-reference.
//
// AuthorizationID is OPTIONAL on the wire, for the same reason as on
// DeleteDocumentResponse: a staging document has a NULL authorizationId column,
// which reads back as the zero UUID. Emitting that as
// "00000000-0000-0000-0000-000000000000" would report an UNAUTHORIZED document
// as if it were authorized under a real (all-zero) policy, so it is omitted.
type DocumentMetaResponse struct {
	ID                string    `json:"id"`
	ExternalID        string    `json:"externalID"`
	MimeType          string    `json:"mimeType"`
	Size              int       `json:"size"`
	DisplayName       string    `json:"displayName"`
	CreatedBy         *string   `json:"createdBy,omitempty"`
	TemporaryLocation bool      `json:"temporaryLocation"`
	StorageBucketID   string    `json:"storageBucketId"`
	AuthorizationID   *string   `json:"authorizationId,omitempty"`
	TagsetID          *string   `json:"tagsetId,omitempty"`
	ExternalReference *string   `json:"externalReference,omitempty"`
	CreatedDate       time.Time `json:"createdDate"`
	UpdatedDate       time.Time `json:"updatedDate"`
	// ImageWidth/ImageHeight are post-rotation pixel dimensions sourced from
	// content_metadata for image rows (both nil for non-images and for image
	// rows whose metadata is empty/sentinel). Carried on /meta and the
	// by-reference response so the Synapse provider gets conversation-attachment
	// dimensions in one lookup.
	ImageWidth  *int `json:"imageWidth,omitempty"`
	ImageHeight *int `json:"imageHeight,omitempty"`
}

// Render writes the response as JSON with HTTP 200.
func (r DocumentMetaResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// DocumentMetaBatchRequest is the body for POST /internal/file/meta-batch.
// IDs is a bounded, non-empty list of document ids; every element must be a
// valid UUID. Duplicates are de-duplicated server-side.
//
// Deliberately NO `apispec:"format=uuid"` tag here, unlike the other
// UUID-valued body fields: the generator applies a field tag to the FIELD's
// schema, which for a slice is the ARRAY — it emitted `format: uuid` next to
// `type: array` rather than inside `items`, an inert keyword in the wrong
// place. apispec v0.4.25 has no items-level tag, so the UUID requirement is
// stated in the handler's doc comment (which IS published, as the operation
// description) instead of mis-declared here.
type DocumentMetaBatchRequest struct {
	IDs []string `json:"ids"`
}

// DocumentMetaBatchResponse is returned by POST /internal/file/meta-batch —
// the batched form of GET /internal/file/{id}/meta, reusing that endpoint's
// exact per-document shape.
//
// Files is a PARTIAL result by design: an id that resolves to no row is simply
// absent (a document may be deleted between the caller's read and this batch),
// which is a 200, not an error. Order is unspecified; the caller maps by id.
type DocumentMetaBatchResponse struct {
	Files []DocumentMetaResponse `json:"files"`
}

// Render writes the response as JSON with HTTP 200.
func (r DocumentMetaBatchResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// UpdateDocumentRequest is the body for PATCH /internal/file/:id — the
// "move + re-attribute" primitive. All fields are optional; at least one must
// be present. Omitted fields retain their current value. mimeType, externalID,
// and size are immutable through this endpoint — see PUT
// /internal/file/{id}/content for content replacement.
//
// authorizationId, createdBy, and externalReference are tri-state: omitted →
// keep; a string → set; explicit JSON null → clear (authorizationId is NOT
// clearable — clearing it would orphan the document). The presence map in the
// handler distinguishes "omitted" from "null" (a plain *string cannot).
//
// The `apispec:"format=uuid"` tags pin the published `format: uuid` on the
// UUID-valued fields. The generator otherwise infers it by following the
// decoded body into a uuid.Parse call, and that flow analysis cannot see
// through the shared parse helpers these fields go through — so the format is
// declared at the source instead of depending on the shape of the handler.
type UpdateDocumentRequest struct {
	StorageBucketID   *string `json:"storageBucketId,omitempty" apispec:"format=uuid"`
	TemporaryLocation *bool   `json:"temporaryLocation,omitempty"`
	DisplayName       *string `json:"displayName,omitempty"`
	AuthorizationID   *string `json:"authorizationId,omitempty" apispec:"format=uuid"`
	CreatedBy         *string `json:"createdBy,omitempty" apispec:"format=uuid"`
	ExternalReference *string `json:"externalReference,omitempty"`
}

// CopyDocumentRequest is the JSON body for POST /internal/file/copy.
// Reuses CreateDocumentResponse for the response shape.
// The `apispec:"format=uuid"` tags on tagsetId/createdBy pin the published
// `format: uuid`: those two reach uuid.Parse through a shared helper the
// generator's flow analysis cannot follow, unlike sourceId/destinationBucketId/
// authorizationId, which it still infers from the handler's inline parses.
type CopyDocumentRequest struct {
	SourceID            string  `json:"sourceId"`
	DestinationBucketID string  `json:"destinationBucketId"`
	AuthorizationID     string  `json:"authorizationId"`
	TagsetID            *string `json:"tagsetId,omitempty" apispec:"format=uuid"`
	CreatedBy           *string `json:"createdBy,omitempty" apispec:"format=uuid"`
	// ExternalReference is the opaque caller reference set on the copied row
	// (the re-share fork carries the same media_id). Omitted leaves it unset.
	ExternalReference *string `json:"externalReference,omitempty"`
	SkipDedup         bool    `json:"skipDedup,omitempty"`
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Status  string            `json:"status"`
	Details map[string]string `json:"details"`
}

// Render writes the response as JSON with the caller-chosen status code —
// 200 when healthy, 503 when a dependency check failed.
func (r HealthResponse) Render(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}
