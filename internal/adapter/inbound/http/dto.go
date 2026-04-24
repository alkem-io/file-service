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
}

func (r CreateDocumentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(r)
}

// DeleteDocumentResponse is returned by DELETE /internal/document/:id.
type DeleteDocumentResponse struct {
	AuthorizationID string  `json:"authorizationId"`
	TagsetID        *string `json:"tagsetId,omitempty"`
}

func (r DeleteDocumentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// UpdateDocumentResponse is returned by PATCH /internal/document/:id.
type UpdateDocumentResponse struct {
	ID                string `json:"id"`
	StorageBucketID   string `json:"storageBucketId"`
	TemporaryLocation bool   `json:"temporaryLocation"`
}

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
}

func (r ReplaceContentResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// DocumentMetaResponse is returned by GET /internal/document/:id/meta.
type DocumentMetaResponse struct {
	ID                string    `json:"id"`
	ExternalID        string    `json:"externalID"`
	MimeType          string    `json:"mimeType"`
	Size              int       `json:"size"`
	DisplayName       string    `json:"displayName"`
	CreatedBy         *string   `json:"createdBy,omitempty"`
	TemporaryLocation bool      `json:"temporaryLocation"`
	StorageBucketID   string    `json:"storageBucketId"`
	AuthorizationID   string    `json:"authorizationId"`
	TagsetID          *string   `json:"tagsetId,omitempty"`
	CreatedDate       time.Time `json:"createdDate"`
	UpdatedDate       time.Time `json:"updatedDate"`
}

func (r DocumentMetaResponse) Render(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(r)
}

// UpdateDocumentRequest is the body for PATCH /internal/document/:id.
type UpdateDocumentRequest struct {
	StorageBucketID   *string `json:"storageBucketId"`
	TemporaryLocation *bool   `json:"temporaryLocation"`
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Status  string            `json:"status"`
	Details map[string]string `json:"details"`
}

func (r HealthResponse) Render(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}
