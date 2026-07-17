package http

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// activeContentMIME is the deny-list of types forced to download via
// Content-Disposition: attachment. These execute script in a browsing
// context, so serving them inline from this origin enables stored-XSS via an
// uploaded file. Everything else — PDFs, plain images, office docs, … — is
// served inline (TS file-service parity), matched against the base MIME with
// parameters stripped. X-Content-Type-Options: nosniff is always set so the
// browser cannot sniff a benign type into an active one.
var activeContentMIME = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"image/svg+xml":         true,
	"application/xml":       true,
	"text/xml":              true,
}

// PublicHandler handles the authenticated public file serving endpoint.
type PublicHandler struct {
	Repo    port.DocumentRepo
	Auth    port.AuthPort
	Storage port.StoragePort
	MaxAge  int
	Logger  *zap.Logger
}

// ServeDocument handles GET /rest/storage/file/{id} (and the /rest/storage/document/{id} back-compat alias)
func (h *PublicHandler) ServeDocument(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	docID, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	// actorID may be empty here: anonymous requests are valid input.
	// The auth-evaluation-service evaluates the document's policy
	// against the (possibly anonymous) caller and returns the decision.
	actorID := GetActorID(r.Context())

	doc, err := h.Repo.GetByID(r.Context(), docID)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to lookup document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Authorization check via h2c HTTP/2 (or NATS fallback). For anonymous
	// callers, actorID is empty — the auth-evaluation-service treats that
	// as "no asserted identity" and matches against global-anonymous
	// credential rules in the document's authorization policy.
	result, err := h.Auth.CheckPrivilege(r.Context(), actorID, "read", doc.AuthorizationID.String())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}
	if !result.Allowed {
		writeJSONError(w, http.StatusForbidden, "insufficient privileges")
		return
	}

	// ETag based on content hash — invalidates when file content changes via store-and-link
	etag := `"` + doc.ExternalID + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Read file from storage
	content, err := h.Storage.Read(doc.ExternalID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "file not found on storage")
		} else {
			h.Logger.Error("failed to read file from storage", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Response headers matching TS file-service
	w.Header().Set("Content-Type", doc.MimeType)
	// Stored-XSS hardening: never let the browser sniff a different type, and
	// force only active-content types (svg+xml, html, xml, …) to download so
	// they cannot execute script in this origin. Everything else is served
	// inline for TS-parity preview (PDFs, plain images, …).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if activeContentMIME[model.NormalizeMIME(doc.MimeType)] {
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Disposition", "inline")
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", h.MaxAge))
	w.Header().Set("Pragma", "public")
	w.Header().Set("Expires", time.Now().Add(time.Duration(h.MaxAge)*time.Second).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etag)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
