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

// safeInlineMIME is the allow-list of types served with Content-Disposition:
// inline. Everything else — notably image/svg+xml and text/html, which execute
// scripts in the serving origin — is forced to download via Content-Disposition:
// attachment, defeating stored-XSS via an uploaded SVG/HTML file. These four
// raster types cannot carry script; the web client renders them via
// <img src=url>, which works regardless of the disposition.
var safeInlineMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
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
	// serve only known-safe raster images inline — force everything else
	// (svg+xml, html, octet-stream, …) to download so it cannot execute script
	// in this origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if safeInlineMIME[model.NormalizeMIME(doc.MimeType)] {
		w.Header().Set("Content-Disposition", "inline")
	} else {
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", h.MaxAge))
	w.Header().Set("Pragma", "public")
	w.Header().Set("Expires", time.Now().Add(time.Duration(h.MaxAge)*time.Second).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etag)

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}
