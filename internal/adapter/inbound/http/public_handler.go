package http

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/port"
)

var ErrDocumentNotFound = errors.New("document not found")

// PublicHandler handles the authenticated public file serving endpoint.
type PublicHandler struct {
	Repo    port.DocumentRepo
	Auth    port.AuthPort
	Storage port.StoragePort
	MaxAge  int
}

// ServeDocument handles GET /rest/storage/document/{id}
func (h *PublicHandler) ServeDocument(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	docID, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	actorID := GetActorID(r.Context())
	if actorID == "" {
		writeJSONError(w, http.StatusUnauthorized, "missing actor identity")
		return
	}

	doc, err := h.Repo.GetByID(r.Context(), docID)
	if err != nil {
		if errors.Is(err, ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to lookup document")
		return
	}

	// ETag conditional request — check before auth to save a NATS call
	if r.Header.Get("If-None-Match") == doc.ID.String() {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Authorization via NATS
	result, err := h.Auth.CheckPrivilege(r.Context(), actorID, "read", doc.AuthorizationID.String())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}
	if !result.Allowed {
		writeJSONError(w, http.StatusForbidden, "insufficient privileges")
		return
	}

	// Read file from storage
	content, err := h.Storage.Read(doc.ExternalID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "file not found on storage")
		return
	}

	// Response headers matching TS file-service
	w.Header().Set("Content-Type", doc.MimeType)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", h.MaxAge))
	w.Header().Set("Pragma", "public")
	w.Header().Set("Expires", time.Now().Add(time.Duration(h.MaxAge)*time.Second).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", doc.ID.String())

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
