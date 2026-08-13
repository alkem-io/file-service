package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// activeContentMIME is the non-XML part of the DENY-list of content-types
// served with Content-Disposition: attachment. This is a deliberate deny-list +
// nosniff design — NOT an allow-list — and it is settled; do not flip it back.
//
// Why a deny-list is the correct model HERE (and secure):
//
//   - X-Content-Type-Options: nosniff is set on EVERY response, so the browser
//     renders strictly by the DECLARED Content-Type. It cannot sniff a benign
//     declared type (e.g. text/plain, image/png) into an active one — the
//     classic path that made allow-listing necessary elsewhere is closed here.
//   - Given nosniff, the ONLY way an uploaded file can execute script in this
//     origin is if its stored/declared Content-Type is itself one a browser
//     will actively render or script from. That set is SMALL and KNOWN: HTML,
//     the whole XML family (see isActiveContentMIME), and the MHTML/rfc822
//     "web archive" types a browser will open as a live page. Forcing those to
//     download is complete for the active-content threat.
//   - Everything OUTSIDE that set is inert media whose worst case under nosniff
//     is "browser downloads it or shows a benign preview": raster/vector-raster
//     images, video, audio, PDF, plain text, JSON, CSV, office documents, and
//     any unknown/octet-stream type. Serving these inline is the TS file-service
//     parity UX (in-browser preview) with no XSS exposure.
//
// The XML family is matched by SUFFIX, not enumerated here — see
// isActiveContentMIME. Matched against the base MIME with parameters stripped
// (NormalizeMIME), case-insensitively.
var activeContentMIME = map[string]bool{
	"text/html":                 true,
	"text/xsl":                  true,
	"message/rfc822":            true,
	"multipart/related":         true,
	"application/x-mimearchive": true,
}

// xmlMIMESuffix is the RFC 6839 structured-syntax suffix that makes a media
// type XML. A browser routes EVERY `*+xml` type through its XML parser, and an
// `<?xml-stylesheet?>` processing instruction there loads XSLT, which can
// execute script in this origin — so the whole family is active content, not
// just the handful of dialects anyone thought to name.
const xmlMIMESuffix = "+xml"

// xmlMIMEBases are the XML media types that carry no `+xml` suffix because they
// ARE the family root (RFC 7303). Everything else XML-ish is caught by the
// suffix.
var xmlMIMEBases = map[string]bool{
	"application/xml": true,
	"text/xml":        true,
}

// isActiveContentMIME reports whether a stored MIME type must be forced to
// download rather than served inline.
//
// The XML family is matched by SUFFIX rather than enumerated: an enumeration is
// unsound here because it can only ever list the dialects someone remembered,
// while this service's own detector (gabriel-vasile/mimetype) emits a dozen
// `+xml` types on its own — application/gml+xml, application/gpx+xml,
// application/owl+xml, application/vnd.garmin.tcx+xml,
// application/vnd.google-earth.kml+xml,
// application/vnd.ms-package.3dmanufacturing-3dmodel+xml,
// application/vnd.ms-visio.drawing.main+xml, application/x-xliff+xml,
// model/vnd.collada+xml, model/x3d+xml, … — and a caller may declare any other
// `+xml` type on an empty upload. Every one of them renders as XML in a browser
// and can therefore carry an XSLT processing instruction.
//
// mimeType is normalized first (parameters stripped, lower-cased, trimmed) so
// `Image/SVG+XML; charset=UTF-8` is recognized exactly like `image/svg+xml`.
func isActiveContentMIME(mimeType string) bool {
	normalized := model.NormalizeMIME(mimeType)
	if strings.HasSuffix(normalized, xmlMIMESuffix) || xmlMIMEBases[normalized] {
		return true
	}
	return activeContentMIME[normalized]
}

// rfc8187AttrChars is the non-alphanumeric part of RFC 8187's attr-char set —
// the bytes an ext-value may carry unencoded. Note what it excludes: quote,
// backslash, semicolon, CR, LF and every other header-significant byte, so a
// value reduced to this set cannot influence header framing.
const rfc8187AttrChars = "!#$&+-.^_`|~"

const upperhex = "0123456789ABCDEF"

// attachmentDisposition builds the Content-Disposition header for a forced
// download, naming the file so it saves under its displayName instead of the
// bare document UUID (with no extension).
//
// displayName is caller-controlled data going into a response HEADER, so it is
// SANITIZED, not merely escaped:
//
//   - filename= (the ASCII fallback) keeps printable ASCII only, minus the
//     quoted-string metacharacters (" and \), the parameter separator (;) and
//     the path separators (/ and \). Every other byte — CR, LF, NUL, any
//     control or non-ASCII byte — becomes '_'. No input can close the quoted
//     string, append a parameter, or start a new header line.
//   - filename* (the RFC 5987/8187 UTF-8 ext-value) carries the exact name for
//     UAs that implement it, percent-encoded down to attr-char, which likewise
//     contains no header-significant byte.
//
// Legacy rows predate validateDisplayName, so the sanitizing is real defence
// rather than belt-and-braces. A blank name yields a bare `attachment`.
func attachmentDisposition(displayName string) string {
	if strings.TrimSpace(displayName) == "" {
		return "attachment"
	}
	return `attachment; filename="` + asciiFilename(displayName) +
		`"; filename*=UTF-8''` + rfc8187Encode(displayName)
}

// asciiFilename reduces a name to bytes that are safe inside a quoted-string
// header parameter, substituting '_' for everything else (see
// attachmentDisposition for the threat model).
func asciiFilename(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i := range len(name) {
		c := name[i]
		switch {
		case c < 0x20 || c >= 0x7f, c == '"', c == '\\', c == ';', c == '/':
			b.WriteByte('_')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// rfc8187Encode percent-encodes a UTF-8 string down to RFC 8187's attr-char
// set, for the filename* parameter value.
func rfc8187Encode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.IndexByte(rfc8187AttrChars, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// applyServeHardening writes the stored-XSS hardening headers for a served
// document: nosniff forces declared-type rendering, and an active-content type
// is forced to download (named by its displayName) instead of rendering inline.
// Single definition so the 200 and 304 paths cannot drift — a conditional
// request must carry the same hardening, because a cache updates its stored
// headers from the 304.
func applyServeHardening(w http.ResponseWriter, doc model.Document) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if isActiveContentMIME(doc.MimeType) {
		w.Header().Set("Content-Disposition", attachmentDisposition(doc.DisplayName))
	} else {
		w.Header().Set("Content-Disposition", "inline")
	}
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

	// A document with NO authorization policy is DENIED here, at the
	// file-service boundary. Its authorizationId column is NULL, which reads
	// back as the zero UUID; handing that to CheckPrivilege would delegate the
	// readability of a policy-less document to whatever the auth-evaluation
	// service does with a policy id that does not exist. Staging documents (the
	// Synapse media provider's matrix_media store) are not publicly servable —
	// the server mints the real policy on inbound re-home, and only then does a
	// privilege evaluation mean anything. Answered with the same 403 as any
	// other denial so the response does not distinguish "policy-less" from
	// "not permitted".
	if doc.AuthorizationID == uuid.Nil {
		writeJSONError(w, http.StatusForbidden, "insufficient privileges")
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
		// A 304 carries no body, but a cache MUST update the stored response's
		// headers from it (RFC 9110 §15.4.5) — so the hardening ships here too.
		// Without it a revalidation would let a cache keep, and keep serving, a
		// stored copy that predates the deny-list/nosniff hardening.
		applyServeHardening(w, doc)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Stream the file from storage (constant memory — never buffer the whole blob, so a burst of
	// concurrent large-file reads can't drive RSS to N×blobsize).
	rc, size, err := h.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		writeStorageReadError(w, h.Logger, err, "file not found on storage", "failed to read file from storage")
		return
	}

	// Response headers matching TS file-service
	w.Header().Set("Content-Type", doc.MimeType)
	applyServeHardening(w, doc)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", h.MaxAge))
	w.Header().Set("Pragma", "public")
	w.Header().Set("Expires", time.Now().Add(time.Duration(h.MaxAge)*time.Second).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

	w.WriteHeader(http.StatusOK)
	streamBlob(w, h.Logger, rc, size, doc.ExternalID)
}
