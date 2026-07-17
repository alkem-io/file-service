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

// activeContentMIME is the DENY-list of content-types served with
// Content-Disposition: attachment. This is a deliberate deny-list + nosniff
// design — NOT an allow-list — and it is settled; do not flip it back.
//
// Why a deny-list is the correct model HERE (and secure):
//
//   - X-Content-Type-Options: nosniff is set on EVERY response, so the browser
//     renders strictly by the DECLARED Content-Type. It cannot sniff a benign
//     declared type (e.g. text/plain, image/png) into an active one — the
//     classic path that made allow-listing necessary elsewhere is closed here.
//   - Given nosniff, the ONLY way an uploaded file can execute script in this
//     origin is if its stored/declared Content-Type is itself one a browser
//     will actively render or script from. That set is SMALL, KNOWN, and
//     enumerable: HTML/XHTML, SVG, the XML family (browsers render/route XML,
//     and XSLT/foreignObject can execute), the syndication/RDF/MathML XML
//     dialects, and the MHTML/rfc822 "web archive" types that a browser will
//     open as a live page. Enumerating those and forcing them to download is
//     complete for the active-content threat.
//   - Everything OUTSIDE that set is inert media whose worst case under nosniff
//     is "browser downloads it or shows a benign preview": raster/vector-raster
//     images (png/jpeg/gif/webp/avif/bmp/tiff/heic), video, audio, PDF, plain
//     text, JSON, CSV, office documents, and any unknown/octet-stream type.
//     Serving these inline is the TS file-service parity UX (in-browser
//     preview) with no XSS exposure.
//
// An allow-list here would be WRONG for UX: it cannot enumerate the open-ended,
// ever-growing set of safe media types, so it force-downloads legitimate
// previewable content (the regressions delta-2 flagged: bmp/tiff/heic, video,
// audio, json, csv, office). The active-content set, by contrast, IS closed and
// enumerable — so the deny-list is both secure (with nosniff) and correct for
// UX. Matched against the base MIME with parameters stripped (NormalizeMIME),
// case-insensitively.
var activeContentMIME = map[string]bool{
	"text/html":                 true,
	"application/xhtml+xml":     true,
	"image/svg+xml":             true,
	"application/xml":           true,
	"text/xml":                  true,
	"application/rss+xml":       true,
	"application/atom+xml":      true,
	"application/rdf+xml":       true,
	"application/mathml+xml":    true,
	"message/rfc822":            true,
	"multipart/related":         true,
	"application/x-mimearchive": true,
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
	// Stored-XSS hardening (deny-list + nosniff; see activeContentMIME).
	// nosniff forces declared-type rendering, so only the enumerable set of
	// browser-active/script-capable types can execute; those download via
	// attachment. Everything else — inert media, documents, unknown types —
	// serves inline for TS-parity in-browser preview.
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
