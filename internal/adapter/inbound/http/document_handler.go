// Package http is the inbound HTTP adapter: chi routing, the internal
// document CRUD + streaming-ingest endpoints, the public file-serve endpoint,
// health/liveness probes, middleware (request ID, actor identity, logging),
// and the expvar resilience metrics. It translates HTTP requests into domain
// service calls and domain errors back into stable status codes and JSON
// bodies; no business rules live here.
package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// DocumentHandler handles all internal document endpoints.
type DocumentHandler struct {
	Service *service.FileService
	MaxAge  int
	Logger  *zap.Logger

	// Streaming-ingest guards (spec 020). Zero values fall back to the
	// historical defaults (32 MiB cap, 30 s idle).
	MaxUploadSize int64
	IdleTimeout   time.Duration
}

// writeLookupError maps a document-lookup failure to its HTTP response: a
// model.ErrDocumentNotFound is a 404 with the "document not found" body; any
// other error is logged (with logMsg) and surfaces as a 500 "internal error".
// Shared by every read path that starts with a repo lookup (GetMeta,
// ByReference, Update) so the mapping can't drift between them.
func (h *DocumentHandler) writeLookupError(w http.ResponseWriter, err error, logMsg string) {
	if errors.Is(err, model.ErrDocumentNotFound) {
		writeJSONError(w, http.StatusNotFound, "document not found")
		return
	}
	h.Logger.Error(logMsg, zap.Error(err))
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

// GetMeta handles GET /internal/file/{id}/meta
func (h *DocumentHandler) GetMeta(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	doc, err := h.Service.Repo.GetByID(r.Context(), docID)
	if err != nil {
		h.writeLookupError(w, err, "failed to lookup document")
		return
	}

	documentMetaResponse(doc).Render(w)
}

// ByReference handles GET /internal/file/by-reference?ref=<v>&bucketId=<uuid?>.
// ref is required. bucketId OMITTED → GLOBAL resolution (the provider's fetch:
// any document carrying the reference, all sharing one blob). bucketId present
// → bucket-SCOPED resolution (read resolution: the document in that bucket).
// 200 → document meta (incl. externalReference + image dims); 404 → no match.
//
// Only an ABSENT bucketId parameter selects the global lookup. A bucketId that
// is PRESENT but empty (`&bucketId=`) is malformed input, not an omission, and
// is rejected with the same 400 as a malformed one — silently widening a
// bucket-scoped read into a cross-bucket one because the caller's variable
// interpolated empty is exactly the failure mode that must not be quiet.
func (h *DocumentHandler) ByReference(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	ref := query.Get("ref")
	if ref == "" {
		writeJSONError(w, http.StatusBadRequest, "missing required query parameter: ref")
		return
	}

	// Read the value and its PRESENCE separately: Get collapses "absent" and
	// "present but empty" to "", and those two mean opposite things here.
	bucketParam := query.Get("bucketId")
	_, bucketPresent := query["bucketId"]

	var (
		doc model.Document
		err error
	)
	if !bucketPresent {
		doc, err = h.Service.Repo.GetByReference(r.Context(), ref)
	} else {
		bucketID, perr := uuid.Parse(bucketParam)
		if perr != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid bucketId")
			return
		}
		doc, err = h.Service.Repo.GetByReferenceInBucket(r.Context(), ref, bucketID)
	}
	if err != nil {
		h.writeLookupError(w, err, "failed to lookup document by reference")
		return
	}

	documentMetaResponse(doc).Render(w)
}

// GetContent handles GET /internal/file/{id}/content
func (h *DocumentHandler) GetContent(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	doc, err := h.Service.Repo.GetByID(r.Context(), docID)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to lookup document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	rc, size, err := h.Service.Storage.ReadStream(doc.ExternalID)
	if err != nil {
		writeStorageReadError(w, h.Logger, err, "file not found on storage", "failed to read file from storage")
		return
	}

	w.Header().Set("Content-Type", doc.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	// Streamed (constant memory) — a stored key, so a malformed one is a 500 (server data), never 400.
	streamBlob(w, h.Logger, rc, size, doc.ExternalID)
}

// GetBlobContent handles GET /internal/blob/{hash}/content — a content-addressed
// read. It returns the immutable blob stored under a SHA3-256 content hash,
// independent of any document row. The store is already content-addressed
// (Storage.Read keys on the hash); this exposes that read over the API.
//
// This is the read path the backup/replication worker uses (workspace#008,
// FR-008): the outbox records an object's content hash, and the blob under a
// hash never changes, so a fetch-by-hash is version-exact — it returns the
// EXACT bytes that were enqueued, never whichever version the (mutable)
// document currently points at, which is what GET /file/{id}/content returns.
// A hash with no blob (e.g. a version superseded and refcount-deleted) is a
// clean 404, distinguishable by the caller from a transport failure.
func (h *DocumentHandler) GetBlobContent(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")

	// Storage owns key validation (one definition — the deliberately legacy-CID-permissive
	// isValidExternalID), so the handler never drifts from what the store serves; the shared
	// writeStorageReadError maps the failure. file-service does NOT try to tell an absent blob
	// from a store outage (storage can't, so it doesn't claim to) — a non-ENOENT backend error
	// is a retryable 500; a 404 is just "no such blob" and the worker decides what that means.
	rc, size, err := h.Service.Storage.ReadStream(hash)
	if err != nil {
		// The key here is the client's URL {hash}, so a malformed one is a 400 (client error).
		// This is the ONLY read path where that's true — GetContent/ServeDocument read a stored
		// key — so the 400 lives here, not in the shared helper (keeps it off their API contract).
		if errors.Is(err, port.ErrInvalidKey) {
			writeJSONError(w, http.StatusBadRequest, "invalid content hash")
			return
		}
		writeStorageReadError(w, h.Logger, err, "blob not found", "failed to read blob from storage")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff") // raw bytes, never a document to render
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	// Streamed (constant memory) so a bulk parallel reader can't drive RSS to N×blobsize; streamBlob
	// classifies a mid-stream failure (backend fault / short read → abort; client disconnect → silent).
	streamBlob(w, h.Logger, rc, size, hash)
}

// readErrCapture records a non-EOF error from the wrapped reader so a caller can tell a source
// (backend) read failure from a destination (client) write failure after io.Copy returns — which
// io.Copy itself does not distinguish.
type readErrCapture struct {
	r   io.Reader
	err error
}

func (c *readErrCapture) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// createFields collects the multipart metadata fields by name (spec 020:
// the fields trail the file part — research R4). Typed struct rather than a
// map: direct field reads keep generated API docs clean (map-index string
// literals are misinferred as path parameters by the spec generator).
type createFields struct {
	displayName         string
	storageBucketID     string
	authorizationID     string
	tagsetID            string
	createdBy           string
	temporaryLocation   string
	skipDedup           string
	allowedMimeTypes    string
	maxFileSize         string
	externalReference   string
	skipImageProcessing string

	// authorizationIDPresent records whether an authorizationId PART APPEARED
	// in the multipart body at all. authorizationId is the one optional field
	// where absent and blank must not be conflated: an OMITTED part means "no
	// server-minted authorization yet" (the Synapse provider's staging store →
	// SQL NULL), while a part that is present but blank is malformed input and
	// must stay the 400 it has always been. Emptiness alone cannot tell those
	// apart, so the collector records presence directly.
	authorizationIDPresent bool
}

// set records one metadata part. An UNRECOGNIZED name is an error, not a
// silent no-op: an exact-match switch that ignores what it doesn't know turns a
// caller typo ("skipimageprocessing", "externalRef") into a 201 that stored the
// wrong thing — a verbatim contract silently transcoded, or a bridge document
// with no reference and therefore unreachable by the provider. This is the same
// choice the JSON paths already make with DisallowUnknownFields: an unknown
// field surfaces as a 400 rather than a successful-but-wrong write. The error
// message is safe as a 400 body (the field NAME comes from the request, the
// value never does).
func (f *createFields) set(name, value string) error {
	switch name {
	case "displayName":
		f.displayName = value
	case "storageBucketId":
		f.storageBucketID = value
	case "authorizationId":
		f.authorizationID = value
		f.authorizationIDPresent = true
	case "tagsetId":
		f.tagsetID = value
	case "createdBy":
		f.createdBy = value
	case "temporaryLocation":
		f.temporaryLocation = value
	case "skipDedup":
		f.skipDedup = value
	case "allowedMimeTypes":
		f.allowedMimeTypes = value
	case "maxFileSize":
		f.maxFileSize = value
	case "externalReference":
		f.externalReference = value
	case "skipImageProcessing":
		f.skipImageProcessing = value
	default:
		return fmt.Errorf("unknown multipart field: %s", sanitizeFieldName(name))
	}
	return nil
}

// sanitizeFieldName reduces an unrecognized multipart field name to printable
// ASCII before it is echoed in a 400 body, so a crafted part name cannot smuggle
// control characters into the response. Over-long names are truncated.
func sanitizeFieldName(name string) string {
	const maxFieldNameLen = 64
	if len(name) > maxFieldNameLen {
		name = name[:maxFieldNameLen]
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := range len(name) {
		if c := name[i]; c < 0x20 || c >= 0x7f {
			b.WriteByte('_')
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// parseOptionalUUID parses an optional UUID field (multipart form value, or a
// JSON string field flattened through derefString): empty means absent
// (nil, nil). The error names the field so it can serve directly as a 400
// body.
//
// A value that IS supplied must be a real, NON-nil UUID. The all-zero UUID
// parses cleanly but is this codebase's SQL-NULL sentinel: uuidToPgxNullable
// writes a LITERAL all-zero id (NOT NULL) for it, and every nullable id column
// reads back through optionalUUIDString, so the row would report an owner /
// tagset of "00000000-…" that matches no actor and no tagset — through meta and
// the by-reference response — while the caller was told it stored the value it
// supplied. Rejecting it keeps ONE rule across create, copy and patch:
// supplied ⇒ valid non-nil UUID; omitted ⇒ NULL. Absence is still how a caller
// says "no owner" (the Synapse media provider sends neither field).
func parseOptionalUUID(value, field string) (*uuid.UUID, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", field)
	}
	if parsed == uuid.Nil {
		return nil, fmt.Errorf("%s cannot be the nil UUID", field)
	}
	return &parsed, nil
}

// parseOptionalBool parses an optional boolean form field: empty means
// false. The error names the field so it can serve directly as a 400 body.
func parseOptionalBool(value, field string) (bool, error) {
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid %s: must be true or false", field)
	}
	return parsed, nil
}

// buildCreateInput validates the collected metadata fields after the upload
// has been staged (research R4).
//
// stagedSkip is the skipImageProcessing value the file part was ACTUALLY staged
// under, and it — not the collected field — is what lands on the input. The
// staged decision is the only one that describes the stored bytes; re-parsing
// the field here would create a second value for the same fact that a duplicate
// part could drive out of agreement with reality (validateSkipAfterFile rejects
// a contradicting duplicate, so the two can no longer diverge at all).
func buildCreateInput(fields createFields, stagedSkip bool) (input model.CreateDocumentInput, allowedMimeTypes []string, maxFileSize int, err error) {
	if err := validateDisplayName(fields.displayName); err != nil {
		return input, nil, 0, err
	}

	storageBucketID, err := uuid.Parse(fields.storageBucketID)
	if err != nil {
		return input, nil, 0, fmt.Errorf("invalid storageBucketId")
	}

	// authorizationId is OPTIONAL by ABSENCE ONLY. The Synapse media storage
	// provider stores staging docs in the reserved matrix_media bucket with NO
	// server-minted authorization — the server mints one on inbound re-home
	// (MOVE into the conversation bucket). An omitted part leaves the zero UUID,
	// which the adapter maps to a NULL column (the column is nullable UNIQUE; a
	// zero UUID for every provider store would collide).
	//
	// A part that IS present must carry a valid, non-nil UUID: a blank /
	// whitespace-only value is malformed input, not "absent", and the literal
	// zero UUID would silently become the same SQL NULL through a caller that
	// clearly believes it is supplying a policy.
	var authorizationID uuid.UUID
	if fields.authorizationIDPresent {
		authorizationID, err = uuid.Parse(fields.authorizationID)
		if err != nil {
			return input, nil, 0, fmt.Errorf("invalid authorizationId")
		}
		if authorizationID == uuid.Nil {
			return input, nil, 0, fmt.Errorf("authorizationId cannot be the nil UUID")
		}
	}

	tagsetID, err := parseOptionalUUID(fields.tagsetID, "tagsetId")
	if err != nil {
		return input, nil, 0, err
	}

	createdBy, err := parseOptionalUUID(fields.createdBy, "createdBy")
	if err != nil {
		return input, nil, 0, err
	}

	temporaryLocation, err := parseOptionalBool(fields.temporaryLocation, "temporaryLocation")
	if err != nil {
		return input, nil, 0, err
	}

	skipDedup, err := parseOptionalBool(fields.skipDedup, "skipDedup")
	if err != nil {
		return input, nil, 0, err
	}

	allowedMimeTypes = parseAllowedMimeTypes(fields.allowedMimeTypes)

	maxFileSize, err = parseMaxFileSize(fields.maxFileSize)
	if err != nil {
		return input, nil, 0, err
	}

	externalReference := optionalString(fields.externalReference)
	if err := validateExternalReference(externalReference); err != nil {
		return input, nil, 0, err
	}

	input = model.CreateDocumentInput{
		DisplayName:         fields.displayName,
		CreatedBy:           createdBy,
		TemporaryLocation:   temporaryLocation,
		StorageBucketID:     storageBucketID,
		AuthorizationID:     authorizationID,
		TagsetID:            tagsetID,
		ExternalReference:   externalReference,
		SkipImageProcessing: stagedSkip,
		SkipDedup:           skipDedup,
	}
	return input, allowedMimeTypes, maxFileSize, nil
}

// parseAllowedMimeTypes splits a comma-separated allow-list into trimmed,
// non-empty entries. An empty input yields a nil slice (no per-request policy
// override).
func parseAllowedMimeTypes(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// parseMaxFileSize parses the optional per-request maxFileSize override. An
// empty input means "no override" (0). A malformed or negative value is a
// 400-safe error.
func parseMaxFileSize(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid maxFileSize: must be a non-negative integer")
	}
	return parsed, nil
}

// optionalString maps a blank field to nil and any other value to a pointer —
// the wire representation of an absent optional string field.
//
// "Blank" is empty OR whitespace-only. A whitespace-only externalReference is
// no more a usable lookup key than an empty one: content-dedup filters IS NULL
// and the by-reference endpoint rejects an empty ref, so persisting "   " would
// orphan the row from both identity systems. The value itself is passed through
// VERBATIM (never trimmed) — externalReference is opaque to file-service, so
// rewriting a caller's key would break their read-back.
func optionalString(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

// derefString returns the pointed-to string, or "" when the pointer is nil —
// the inverse of optionalString, for JSON bodies where an absent optional
// string field decodes to a nil *string.
func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// maxExternalReferenceLen bounds externalReference to the file."externalReference"
// VARCHAR(256) column. Validated on every path that accepts a reference
// (create, copy, patch), so an over-length value is a clean 400 rather than a
// Postgres "value too long" 500. Byte length is used deliberately: capping at
// bytes guarantees the value fits the character-based column regardless of
// UTF-8 expansion.
const maxExternalReferenceLen = 256

// validateStorableText rejects bytes no Postgres CHARACTER column can hold:
// text/varchar are character data, so a value that is not valid UTF-8, or that
// contains a NUL (U+0000, which no Postgres text value may carry at all), is
// rejected by the server on INSERT/UPDATE. Without an up-front check that
// rejection lands as a 500 — and on create it lands AFTER the blob has already
// been published, leaving an orphan blob behind for a request the caller can
// never succeed at. field names the offending input so the message is a safe
// 400 body.
//
// Single-sourced across every caller-supplied text column (externalReference,
// displayName): the rule belongs to the column type, not to one field.
func validateStorableText(field, v string) error {
	if !utf8.ValidString(v) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("%s must not contain NUL characters", field)
	}
	return nil
}

// validateExternalReference rejects a reference the file."externalReference"
// text column cannot hold. ref is the already-normalized optional value
// (nil = no reference).
//
// Length is only half of it; the other half is the shared storability rule,
// applied up front on every path that accepts a reference (create, copy,
// patch), so malformed input is a clean 400 with no side effects.
//
// Both flavors are reachable: a multipart create carries arbitrary bytes, and a
// JSON body can spell one with a \u0000 escape.
func validateExternalReference(ref *string) error {
	if ref == nil {
		return nil
	}
	if len(*ref) > maxExternalReferenceLen {
		return fmt.Errorf("externalReference exceeds maximum length of %d bytes", maxExternalReferenceLen)
	}
	return validateStorableText("externalReference", *ref)
}

// nonNilUUID maps the zero UUID — how a NULL authorizationId column reads back
// on the Document — to nil, so a PATCH that doesn't touch authorizationId seeds
// the nullable update with NULL rather than rewriting the row to the zero UUID
// (which would collide on UNIQUE("authorizationId")). Symmetric with the
// CreatedBy seed, which is already *uuid.UUID.
func nonNilUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// optionalUUIDString renders an optional UUID for the wire: nil stays nil, so
// the JSON field is OMITTED rather than serialized. Composed with nonNilUUID it
// is also how a NULL authorizationId — which reads back on the Document as the
// zero UUID — is kept off the response: emitting
// "00000000-0000-0000-0000-000000000000" would describe a policy-less staging
// document as authorized under an all-zero policy.
func optionalUUIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// newCreateDocumentResponse builds the shared create/copy success body from a
// materialized document row.
func newCreateDocumentResponse(doc *model.Document) CreateDocumentResponse {
	return CreateDocumentResponse{
		ID:          doc.ID.String(),
		ExternalID:  doc.ExternalID,
		MimeType:    doc.MimeType,
		Size:        doc.Size,
		Reused:      doc.Reused,
		ImageWidth:  doc.ImageWidth,
		ImageHeight: doc.ImageHeight,
	}
}

// writeIngestTransportError maps a streaming transport failure to its HTTP
// response and outcome counter (spec 020 FR-008).
func (h *DocumentHandler) writeIngestTransportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrOverLimit):
		IngestOutcomes.Add("rejected_over_limit", 1)
		writeJSONError(w, http.StatusRequestEntityTooLarge, "upload exceeds the configured size limit")
	case errors.Is(err, service.ErrStalled):
		IngestOutcomes.Add("stalled", 1)
		writeJSONError(w, http.StatusRequestTimeout, "upload stalled: no bytes received within the idle timeout")
	case errors.As(err, new(*service.MimeMismatchError)), errors.Is(err, service.ErrEmptyContent):
		// replace-path semantics handled by the caller; not transport
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	default:
		// Client went away (connection reset, unexpected EOF, multipart
		// truncation): nothing useful to send, but try.
		IngestOutcomes.Add("client_abort", 1)
		writeJSONError(w, http.StatusBadRequest, "upload aborted before completion")
	}
}

// stageFilePart parses the skipImageProcessing flag currently in effect and
// stages the file part's bytes. The verbatim (raw-store) decision is taken at
// stage time, so skipImageProcessing MUST precede the file part in the
// multipart body (the provider sends metadata first); a skipImageProcessing=true
// arriving AFTER the file is rejected by validateSkipAfterFile rather than
// silently transcoded. Returns the staged upload and the skip value that was in
// effect, or ok=false (after writing the error response) on failure.
func (h *DocumentHandler) stageFilePart(w http.ResponseWriter, r *http.Request, part *multipart.Part, fields createFields) (staged *service.StagedUpload, skip, ok bool) {
	skipImageProcessing, perr := parseOptionalBool(fields.skipImageProcessing, "skipImageProcessing")
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return nil, false, false
	}
	declaredMIME := part.Header.Get("Content-Type")
	staged, err := h.Service.StageUpload(r.Context(), part, declaredMIME, skipImageProcessing)
	if err != nil {
		h.writeStageError(w, err)
		return nil, false, false
	}
	return staged, skipImageProcessing, true
}

// validateSkipAfterFile enforces the verbatim-store contract guard (spec 013):
// the file part is staged under the skipImageProcessing value in effect at that
// moment (stagedSkip), and that decision is irreversible — the bytes are already
// written. So a skipImageProcessing part arriving AFTER the file may only RESTATE
// that value; any part that disagrees with it is rejected rather than accepted
// into a request record that contradicts what was actually stored.
//
//   - true after a non-verbatim stage: the bytes may already have been
//     transcoded/rotated, so the byte-exact contract cannot be honored.
//   - false after a verbatim stage: the bytes were stored verbatim; accepting
//     this would report a processed store for content that was never processed.
//
// Returns false after writing the error response.
func (h *DocumentHandler) validateSkipAfterFile(w http.ResponseWriter, fields createFields, staged *service.StagedUpload, stagedSkip bool, part *multipart.Part) bool {
	if staged == nil || part.FormName() != "skipImageProcessing" {
		return true
	}
	skip, perr := parseOptionalBool(fields.skipImageProcessing, "skipImageProcessing")
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return false
	}
	if skip && !stagedSkip {
		writeJSONError(w, http.StatusBadRequest, "skipImageProcessing must be sent before the file part")
		return false
	}
	if !skip && stagedSkip {
		writeJSONError(w, http.StatusBadRequest,
			"skipImageProcessing contradicts the value the file part was staged under")
		return false
	}
	return true
}

// Create handles POST /internal/file — streaming ingest (spec 020): the
// file part flows request → sniff → (transcode) → staged storage without
// whole-file buffering. Parts are processed in whatever order they arrive;
// metadata validation always happens after the loop, before the stage is
// published. (The known caller sends the file part first — research R4 —
// which is why bucket-level limits cannot gate the stream early.)
//
// One ordering dependency exists (spec 013): skipImageProcessing is consumed
// when the file part is staged, so the verbatim/byte-exact contract can only
// be honored if it precedes the file. A skipImageProcessing=true that arrives
// AFTER the file part is rejected with 400 rather than silently transcoded.
func (h *DocumentHandler) Create(w http.ResponseWriter, r *http.Request) {
	capBytes := h.MaxUploadSize
	if capBytes <= 0 {
		capBytes = 32 << 20
	}
	idle := h.IdleTimeout
	if idle <= 0 {
		idle = 30 * time.Second
	}
	pr := newProgressReader(w, r.Body, capBytes, idle)
	defer func() { _ = pr.Close() }()
	r.Body = pr

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	var fields createFields
	var staged *service.StagedUpload
	// stagedSkip records the skipImageProcessing value that was in effect when
	// the file part was staged. A later duplicate skipImageProcessing part
	// consistent with this value is harmless; only a skipImageProcessing=true
	// that is FIRST established after the file part violates the byte-exact
	// contract (see validateSkipAfterFile).
	var stagedSkip bool
	defer func() {
		// Any non-success exit path discards the stage (FR-006); Discard
		// after CompleteUpload is a no-op.
		staged.Discard()
	}()

	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			h.writeIngestTransportError(w, perr)
			return
		}

		if part.FormName() == "file" {
			if staged != nil {
				writeJSONError(w, http.StatusBadRequest, "duplicate file part")
				return
			}
			newStaged, skip, ok := h.stageFilePart(w, r, part, fields)
			if !ok {
				return
			}
			staged, stagedSkip = newStaged, skip
		} else {
			if !h.readMetadataField(w, &fields, part) {
				return
			}
			if !h.validateSkipAfterFile(w, fields, staged, stagedSkip, part) {
				return
			}
		}
		_ = part.Close()
	}

	if staged == nil {
		writeJSONError(w, http.StatusBadRequest, "missing file part")
		return
	}

	input, allowedMimeTypes, maxFileSize, err := buildCreateInput(fields, stagedSkip)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.Service.CompleteUpload(r.Context(), staged, input, allowedMimeTypes, maxFileSize)
	if err != nil {
		h.writeCompleteUploadError(w, err)
		return
	}

	IngestOutcomes.Add("accepted", 1)
	newCreateDocumentResponse(doc).Render(w)
}

// readMetadataField reads one trailing (non-file) multipart part into
// fields, enforcing the per-field size cap. 16 KiB is ample for every
// metadata field (the largest legit value, a long allowedMimeTypes list, is
// ~2.5 KiB) and bounds per-request abuse surface. It reads one byte past
// the cap so an oversized field is REJECTED rather than silently truncated
// into a different request. Reports false after writing the error response
// itself.
func (h *DocumentHandler) readMetadataField(w http.ResponseWriter, fields *createFields, part *multipart.Part) bool {
	const maxCreateFieldBytes = 16 << 10
	b, err := io.ReadAll(io.LimitReader(part, maxCreateFieldBytes+1))
	if err != nil {
		h.writeIngestTransportError(w, err)
		return false
	}
	if len(b) > maxCreateFieldBytes {
		writeJSONError(w, http.StatusBadRequest, sanitizeFieldName(part.FormName())+" exceeds the 16 KiB field limit")
		return false
	}
	if err := fields.set(part.FormName(), string(b)); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// writeCompleteUploadError maps a CompleteUpload failure to its HTTP
// response and outcome counter (spec 020 FR-008): bucket-policy rejections
// (size, MIME) are 413/415, a SkipDedup content collision is 409, anything
// else is a logged 500.
func (h *DocumentHandler) writeCompleteUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrPayloadTooLarge):
		IngestOutcomes.Add("rejected_bucket_policy", 1)
		writeJSONError(w, http.StatusRequestEntityTooLarge, "file too large")
	case errors.Is(err, service.ErrUnsupportedMediaType):
		IngestOutcomes.Add("rejected_bucket_policy", 1)
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported media type")
	case errors.Is(err, service.ErrConflict):
		writeJSONError(w, http.StatusConflict, "skipDedup requested but a row with this content already exists in the bucket")
	default:
		IngestOutcomes.Add("failed_mid_stream", 1)
		h.Logger.Error("failed to create document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeStageError maps a StageUpload failure to its HTTP response and
// outcome counter (spec 020 FR-008).
func (h *DocumentHandler) writeStageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrOverLimit), errors.Is(err, service.ErrStalled):
		h.writeIngestTransportError(w, err)
	case errors.Is(err, port.ErrPixelBudgetExceeded):
		IngestOutcomes.Add("rejected_pixel_budget", 1)
		RejectedContentResponse{Code: "PIXEL_BUDGET_EXCEEDED", Error: err.Error()}.Render(w)
	case errors.Is(err, service.ErrImageProcessing):
		IngestOutcomes.Add("failed_mid_stream", 1)
		writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		if isClientStreamError(err) {
			IngestOutcomes.Add("client_abort", 1)
			writeJSONError(w, http.StatusBadRequest, "upload aborted before completion")
		} else {
			IngestOutcomes.Add("failed_mid_stream", 1)
			h.Logger.Error("failed to stage upload", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
	}
}

// isClientStreamError reports request-stream failures attributable to the
// client (abort, reset, truncated multipart) rather than to the service.
func isClientStreamError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		strings.Contains(err.Error(), "connection reset") ||
		strings.Contains(err.Error(), "client disconnected") ||
		strings.Contains(err.Error(), "multipart")
}

// Copy handles POST /internal/file/copy.
// Materializes a new file row in another bucket that references the same
// content as the source. No bytes traverse the wire — content is
// content-addressed, so the new row simply points at the existing blob.
//
// Per-bucket dedup applies by default; SkipDedup=true forces a fresh row.
// On dedup hit, caller-supplied authorizationId/tagsetId are ignored
// (existing row is authoritative), matching the createDocument contract.
func (h *DocumentHandler) Copy(w http.ResponseWriter, r *http.Request) {
	var body CopyDocumentRequest
	if _, ok := decodeStrictJSON(w, r, &body); !ok {
		return
	}

	sourceID, err := uuid.Parse(body.SourceID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid sourceId")
		return
	}
	destBucketID, err := uuid.Parse(body.DestinationBucketID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid destinationBucketId")
		return
	}
	authID, err := uuid.Parse(body.AuthorizationID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid authorizationId")
		return
	}
	// authorizationId is REQUIRED on copy — a re-share materializes a row the
	// server has already minted a policy for. The zero UUID parses cleanly but
	// is the adapter's SQL-NULL sentinel, so accepting it would silently
	// materialize a policy-less row through an endpoint that has no staging
	// semantics. Reject it explicitly rather than storing NULL.
	if authID == uuid.Nil {
		writeJSONError(w, http.StatusBadRequest, "authorizationId cannot be the nil UUID")
		return
	}

	tagsetID, err := parseOptionalUUID(derefString(body.TagsetID), "tagsetId")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	createdBy, err := parseOptionalUUID(derefString(body.CreatedBy), "createdBy")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Normalize an empty externalReference to "no reference" (NULL), exactly
	// like Create, so a Copy never persists a literal '' that would orphan the
	// row from both identity systems (content-dedup filters IS NULL, by-reference
	// rejects empty).
	externalReference := optionalString(derefString(body.ExternalReference))
	if err := validateExternalReference(externalReference); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	input := model.CopyDocumentInput{
		DestinationBucketID: destBucketID,
		AuthorizationID:     authID,
		TagsetID:            tagsetID,
		CreatedBy:           createdBy,
		ExternalReference:   externalReference,
		SkipDedup:           body.SkipDedup,
	}

	doc, err := h.Service.CopyDocument(r.Context(), sourceID, input)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrDocumentNotFound):
			writeJSONError(w, http.StatusNotFound, "source document not found")
		case errors.Is(err, service.ErrConflict):
			// A unique-constraint collision in the destination bucket: a
			// SkipDedup=true content collision, or a reference-bearing copy that
			// collides on the partial UNIQUE(externalReference, storageBucketId).
			writeJSONError(w, http.StatusConflict, "a conflicting document already exists (unique constraint)")
		default:
			h.Logger.Error("failed to copy document", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	newCreateDocumentResponse(doc).Render(w)
}

// Delete handles DELETE /internal/file/{id}
func (h *DocumentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	deleted, err := h.Service.DeleteDocument(r.Context(), docID)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to delete document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	DeleteDocumentResponse{
		AuthorizationID: optionalUUIDString(nonNilUUID(deleted.AuthorizationID)),
		TagsetID:        optionalUUIDString(deleted.TagsetID),
	}.Render(w)
}

// Update handles PATCH /internal/file/{id}.
// Mutates the "move + re-attribute" fields: storageBucketId, temporaryLocation,
// displayName, authorizationId, createdBy, and externalReference. Each field is
// optional; at least one must produce an effective change. Omitted fields retain
// their current value; createdBy and externalReference accept an explicit JSON
// null to clear (authorizationId is NOT clearable — clearing it orphans the
// document from its policy).
//
// displayName notes:
//   - Validation rejects empty/whitespace-only, length > 512 (matches
//     the file."displayName" VARCHAR(512) column), path separators, and
//     control characters.
//   - mimeType is immutable on this endpoint, so callers renaming a file
//     are responsible for keeping the extension consistent with mimeType.
//     This service does not parse extensions or enforce mimeType matching.
//   - displayName is not part of any uniqueness/dedup index, so renames
//     never conflict at the DB level.
func (h *DocumentHandler) Update(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	body, present, ok := h.decodeAndValidateUpdate(w, r)
	if !ok {
		return
	}

	doc, err := h.Service.Repo.GetByID(r.Context(), docID)
	if err != nil {
		h.writeLookupError(w, err, "failed to lookup document")
		return
	}

	meta, applied, err := buildMetadataUpdate(doc, body, present)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// No field carries an effective change — either every key is a no-op
	// explicit-null (e.g. {"temporaryLocation":null}) or sets a field to its
	// current value (e.g. {"displayName":"<current name>"}). This is an
	// IDEMPOTENT SUCCESS, not an error: a PATCH that produces no effective
	// change returns 200 with the CURRENT document and writes NOTHING. That
	// avoids bumping version+updatedDate on an unchanged row (so a concurrent
	// actor is never spuriously 409'd) while still giving idempotent/desired-
	// state callers a 200 rather than a 400. A structurally empty body ({} with
	// no keys) is still a 400 above (len(present) == 0); only keys-present-but-
	// no-change lands here. Dims come straight off the loaded row's
	// content_metadata (populated by the sweep-dims job, not lazily here).
	//
	// Both outcomes render the SAME body from one tail call, so the no-write and
	// the wrote-then-reloaded 200 can never drift in shape.
	result := &doc
	if applied > 0 {
		result, err = h.Service.UpdateDocumentMetadata(r.Context(), doc, meta)
		if err != nil {
			h.writeUpdateError(w, err)
			return
		}
	}

	newUpdateDocumentResponse(result).Render(w)
}

// writeUpdateError maps an UpdateDocumentMetadata failure to its HTTP response.
func (h *DocumentHandler) writeUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrConflict):
		writeJSONError(w, http.StatusConflict, "document was modified concurrently, retry with fresh version")
	case errors.Is(err, model.ErrDuplicateKey):
		// PATCH never changes externalID, so the collision is not "same
		// content": with the (externalReference, storageBucketId) index a
		// move can collide on reference, and the authorizationId unique
		// constraint can collide on re-attribution. Keep the message
		// generic rather than naming the wrong cause.
		writeJSONError(w, http.StatusConflict, "update conflicts with an existing document in the destination bucket (duplicate reference or authorization)")
	default:
		h.Logger.Error("failed to update document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

// decodeAndValidateUpdate decodes the PATCH body, rejects a structurally empty
// patch, and validates the fields that can be checked without loading the row
// (displayName, and the tri-state externalReference: absent = keep, explicit
// null = clear, empty string = rejected). Returns false after writing the error
// response.
func (h *DocumentHandler) decodeAndValidateUpdate(w http.ResponseWriter, r *http.Request) (UpdateDocumentRequest, map[string]struct{}, bool) {
	var body UpdateDocumentRequest
	present, ok := decodeUpdateRequest(w, r, &body)
	if !ok {
		return body, nil, false
	}

	if !suppliesUpdatableField(body, present) {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return body, nil, false
	}

	if body.DisplayName != nil {
		if err := validateDisplayName(*body.DisplayName); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return body, nil, false
		}
	}

	// externalReference is tri-state: absent = keep, explicit null = clear. A
	// BLANK *string value* (empty or whitespace-only) is neither, so it is
	// rejected rather than silently mapped to NULL (clearing is done with an
	// explicit null). This keeps PATCH from ever persisting a non-NULL blank
	// reference, matching what optionalString normalizes away on create/copy.
	if _, ok := present["externalReference"]; ok && body.ExternalReference != nil && strings.TrimSpace(*body.ExternalReference) == "" {
		writeJSONError(w, http.StatusBadRequest, "externalReference must not be empty")
		return body, nil, false
	}
	if err := validateExternalReference(body.ExternalReference); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return body, nil, false
	}

	return body, present, true
}

// newUpdateDocumentResponse single-sources the model.Document -> PATCH response
// mapping shared by the no-op (200 idempotent) and effective-change paths, so
// the two can't drift on the returned shape (incl. surfaced image dims).
func newUpdateDocumentResponse(doc *model.Document) UpdateDocumentResponse {
	return UpdateDocumentResponse{
		ID:                doc.ID.String(),
		StorageBucketID:   doc.StorageBucketID.String(),
		TemporaryLocation: doc.TemporaryLocation,
		DisplayName:       doc.DisplayName,
		ImageWidth:        doc.ImageWidth,
		ImageHeight:       doc.ImageHeight,
	}
}

// validateDisplayName rejects renames that would corrupt the row or
// produce a filename consumers can't safely use. Shared by POST (create)
// and PATCH (update) so both paths apply the same rules.
//
// The 512-byte cap is intentionally tighter than file."displayName"
// VARCHAR(512), which is character-based in Postgres: capping at bytes
// guarantees any accepted value fits regardless of UTF-8 expansion.
//
// Storability is checked FIRST, and it is the same shared rule externalReference
// applies — file."displayName" is character data too. Nothing below can stand in
// for it: `for _, r := range name` DECODES an invalid byte to U+FFFD (> 0x20),
// so an invalid-UTF-8 name sails past the control-character rule and is only
// rejected by Postgres on INSERT — a 500 that, on create, lands after the blob
// is published and orphans it. 013's inbound re-home sends displayName, so this
// is reachable, and the multipart create path carries arbitrary bytes.
func validateDisplayName(name string) error {
	if err := validateStorableText("displayName", name); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("displayName must not be empty or whitespace-only")
	}
	if len(name) > 512 {
		return fmt.Errorf("displayName exceeds maximum length of 512 bytes")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("displayName must not contain path separators ('/' or '\\')")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("displayName must not contain control characters")
		}
	}
	return nil
}

// ReplaceContent handles PUT /internal/file/{id}/content (store-and-link)
func (h *DocumentHandler) ReplaceContent(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	capBytes := h.MaxUploadSize
	if capBytes <= 0 {
		capBytes = 32 << 20
	}
	idle := h.IdleTimeout
	if idle <= 0 {
		idle = 30 * time.Second
	}
	pr := newProgressReader(w, r.Body, capBytes, idle)
	defer func() { _ = pr.Close() }()

	// ReplaceContent note: if the new content's hash matches another file row in
	// the same bucket, the unique(externalID, storageBucketID) index is violated.
	// The service returns ErrConflict → 409 in that case. UpdateFile (PATCH) does
	// not touch externalID, so it cannot trigger this.
	result, err := h.Service.StoreAndLinkStream(r.Context(), docID, pr)
	if err != nil {
		var mismatch *service.MimeMismatchError
		switch {
		case errors.Is(err, service.ErrOverLimit), errors.Is(err, service.ErrStalled):
			h.writeIngestTransportError(w, err)
		case errors.Is(err, port.ErrPixelBudgetExceeded):
			IngestOutcomes.Add("rejected_pixel_budget", 1)
			RejectedContentResponse{Code: "PIXEL_BUDGET_EXCEEDED", Error: err.Error()}.Render(w)
		case errors.Is(err, model.ErrDocumentNotFound):
			writeJSONError(w, http.StatusNotFound, "document not found")
		case errors.Is(err, service.ErrEmptyContent):
			ReplaceOutcomes.Add(service.ReplaceOutcomeRejectedEmpty, 1)
			RejectedContentResponse{Code: "EMPTY_CONTENT", Error: err.Error()}.Render(w)
		case errors.As(err, &mismatch):
			ReplaceOutcomes.Add(service.ReplaceOutcomeRejectedMismatch, 1)
			RejectedContentResponse{
				Code:  "MIME_MISMATCH",
				Error: err.Error(),
				Detail: &MimeMismatchDetail{
					KnownMime:    mismatch.Known,
					DetectedMime: mismatch.Detected,
				},
			}.Render(w)
		case errors.Is(err, service.ErrImageProcessing):
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeJSONError(w, http.StatusConflict, "document content changed concurrently or conflicts with another document in this bucket")
		case isClientStreamError(err):
			IngestOutcomes.Add("client_abort", 1)
			writeJSONError(w, http.StatusBadRequest, "upload aborted before completion")
		default:
			h.Logger.Error("failed to replace content", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if result.ReplaceOutcome != "" {
		ReplaceOutcomes.Add(result.ReplaceOutcome, 1)
	}

	ReplaceContentResponse{
		ExternalID:  result.ExternalID,
		MimeType:    result.MimeType,
		Size:        result.Size,
		ImageWidth:  result.ImageWidth,
		ImageHeight: result.ImageHeight,
	}.Render(w)
}

func parseDocID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "id"))
}

// maxJSONBodyBytes caps every JSON request body (copy, patch). These are small
// metadata documents; 1 MiB is far above any legitimate one and bounds the
// io.ReadAll below. Blob content NEVER travels on a JSON path — it is streamed
// (multipart create, raw PUT) — so this cap is not a file-size limit.
const maxJSONBodyBytes = 1 << 20

// decodeStrictJSON decodes the request body into dst, rejecting unknown
// fields and any trailing data after the first JSON object. It reports ok=true
// on success, returning the RAW body so a caller that must distinguish "key
// absent" from "key present and null" (the PATCH tri-state fields) can re-scan
// it without a second decoder implementation. On failure it returns ok=false
// AFTER writing the error response itself, so callers must simply return
// without touching w further.
//
// DisallowUnknownFields is load-bearing: immutable fields (e.g. mimeType on
// PATCH) must surface as a 400 rather than silently no-op.
func decodeStrictJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) (raw []byte, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds the 1 MiB limit")
			return nil, false
		}
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	if dec.More() {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: trailing data after first object")
		return nil, false
	}
	return raw, true
}

// decodeUpdateRequest strict-decodes the PATCH body into dst and returns the
// set of top-level keys that were actually present, so the handler can tell
// "field omitted" (keep) from "field explicitly null" (clear) for the
// tri-state fields. On any malformed input it writes the error response itself
// and reports ok=false. The decode/size/unknown-field/trailing-data rules are
// decodeStrictJSON's — this only adds the key-presence scan on top.
func decodeUpdateRequest(w http.ResponseWriter, r *http.Request, dst *UpdateDocumentRequest) (present map[string]struct{}, ok bool) {
	raw, ok := decodeStrictJSON(w, r, dst)
	if !ok {
		return nil, false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	present = make(map[string]struct{}, len(keys))
	for k := range keys {
		present[canonicalPatchKey(k)] = struct{}{}
	}
	return present, true
}

// patchFieldNames is the full set of canonical JSON keys of UpdateDocumentRequest.
// json.Decode fills the struct fields CASE-INSENSITIVELY (so "AuthorizationId"
// sets body.AuthorizationID), so the present-map — which the tri-state fields
// consult to tell "omitted" (keep) from "explicit null" (clear) — must resolve
// each raw key the SAME way. Recording presence under the raw casing instead
// would let a case-variant key set the struct field yet read as ABSENT, silently
// dropping the re-attribute/clear it requested (a half-applied, security-relevant
// re-home).
var patchFieldNames = []string{
	"storageBucketId",
	"temporaryLocation",
	"displayName",
	"authorizationId",
	"createdBy",
	"externalReference",
}

// triStatePatchFields are the PATCH fields where an explicit JSON null is
// MEANINGFUL — a request to clear (createdBy, externalReference) or, for
// authorizationId, a request that is answered with a 400 because clearing it
// would orphan the document. On the remaining fields (storageBucketId,
// temporaryLocation, displayName) a null carries no instruction at all.
var triStatePatchFields = []string{
	"authorizationId",
	"createdBy",
	"externalReference",
}

// suppliesUpdatableField reports whether the PATCH body actually SUPPLIES a
// field to update: a value on any of the plain fields, or the presence of any
// tri-state key (whose null is itself an instruction).
//
// A body that supplies nothing — `{}`, or one whose only keys are nulls on the
// plain fields such as {"displayName":null} — is a 400, not a success. That is
// deliberately distinct from the no-op 200 further down the handler, which is
// for a PATCH that NAMES real values which happen to equal the row's current
// ones (so concurrent re-homes don't spuriously 409 each other). Supplying no
// updatable field is a malformed request; supplying one that is already
// satisfied is idempotent success.
func suppliesUpdatableField(body UpdateDocumentRequest, present map[string]struct{}) bool {
	if body.StorageBucketID != nil || body.TemporaryLocation != nil || body.DisplayName != nil {
		return true
	}
	for _, name := range triStatePatchFields {
		if _, ok := present[name]; ok {
			return true
		}
	}
	return false
}

// canonicalPatchKey maps a raw PATCH JSON key to the canonical field name it
// resolves to under json.Decode's case-insensitive struct matching, so
// present-detection agrees with what the decode already applied to the struct.
// A key matching no known field is returned unchanged: DisallowUnknownFields
// has already rejected any genuinely unknown key, and the present-map is only
// ever looked up by canonical name.
func canonicalPatchKey(raw string) string {
	for _, name := range patchFieldNames {
		if strings.EqualFold(raw, name) {
			return name
		}
	}
	return raw
}

// buildMetadataUpdate merges the PATCH fields over the row's current values to
// produce the full DocumentMetadataUpdate the move primitive persists. Fields
// present in the body win; omitted fields keep what the document already has.
// authorizationId/createdBy/externalReference are the re-attribute fields;
// createdBy/externalReference honor explicit JSON null as "clear". A malformed
// UUID yields an error whose message is safe as a 400 body.
//
// applied reports how many fields carry an EFFECTIVE change — a new value that
// differs from the row's current value — so the handler can treat a no-op PATCH
// as an idempotent 200 no-write instead of bumping version+updatedDate on an
// unchanged row. doc is the freshly loaded row, so the comparison needs no
// extra DB round-trip.
func buildMetadataUpdate(doc model.Document, body UpdateDocumentRequest, present map[string]struct{}) (meta model.DocumentMetadataUpdate, applied int, err error) {
	meta = model.DocumentMetadataUpdate{
		StorageBucketID:   doc.StorageBucketID,
		TemporaryLocation: doc.TemporaryLocation,
		DisplayName:       doc.DisplayName,
		AuthorizationID:   nonNilUUID(doc.AuthorizationID),
		CreatedBy:         doc.CreatedBy,
		ExternalReference: doc.ExternalReference,
	}

	if body.StorageBucketID != nil {
		parsed, perr := uuid.Parse(*body.StorageBucketID)
		if perr != nil {
			return meta, applied, fmt.Errorf("invalid storageBucketId")
		}
		if parsed != meta.StorageBucketID {
			meta.StorageBucketID = parsed
			applied++
		}
	}
	if body.TemporaryLocation != nil && *body.TemporaryLocation != meta.TemporaryLocation {
		meta.TemporaryLocation = *body.TemporaryLocation
		applied++
	}
	if body.DisplayName != nil && *body.DisplayName != meta.DisplayName {
		meta.DisplayName = *body.DisplayName
		applied++
	}
	// authorizationId may be RE-ATTRIBUTED to a new policy (part of re-home) but
	// never CLEARED: a NULL/nil authorizationId reads back as the nil UUID,
	// matches no policy, and permanently orphans the document (403 on every
	// read). clearable=false rejects an explicit null or the nil UUID, preserving
	// the invariant that a metadata update can't break authorization.
	_, hasAuth := present["authorizationId"]
	authVal, authChanged, err := applyReattributeUUID(hasAuth, body.AuthorizationID, meta.AuthorizationID, false, "authorizationId")
	if err != nil {
		return meta, applied, err
	}
	if authChanged {
		meta.AuthorizationID = authVal
		applied++
	}
	// createdBy is clearable (explicit null → NULL).
	_, hasCreatedBy := present["createdBy"]
	createdByVal, createdByChanged, err := applyReattributeUUID(hasCreatedBy, body.CreatedBy, meta.CreatedBy, true, "createdBy")
	if err != nil {
		return meta, applied, err
	}
	if createdByChanged {
		meta.CreatedBy = createdByVal
		applied++
	}
	if _, ok := present["externalReference"]; ok {
		if !equalPtr(body.ExternalReference, meta.ExternalReference) {
			meta.ExternalReference = body.ExternalReference // value, or nil for explicit null → clear
			applied++
		}
	}

	return meta, applied, nil
}

// applyReattributeUUID resolves a tri-state optional-UUID PATCH field against
// the row's current value, factoring the shared shape of the authorizationId
// and createdBy re-attribute blocks. present is whether the JSON key was sent;
// raw is the decoded pointer (nil = explicit JSON null); current is the row's
// current value. clearable governs whether an explicit null is allowed:
// authorizationId is NOT clearable (a null/nil UUID orphans the document), so
// clearable=false rejects an explicit null. Returns the resolved value, whether
// it is an EFFECTIVE change vs current, and a 400-safe error naming the field.
//
// The nil UUID is rejected as a VALUE on BOTH fields, clearable or not. Clearing
// is expressed with an explicit JSON null and nothing else: the all-zero UUID is
// the adapter's SQL-NULL sentinel on the way in and reads back as "no value" on
// the way out, so accepting it as a value would persist an id that matches no
// policy and no actor while reporting success to a caller that clearly believed
// it was supplying one. That is exactly the sentinel leak this branch closed on
// create and copy, and PATCH must not reopen it.
func applyReattributeUUID(present bool, raw *string, current *uuid.UUID, clearable bool, field string) (val *uuid.UUID, changed bool, err error) {
	if !present {
		return current, false, nil
	}
	if raw == nil {
		if !clearable {
			return current, false, fmt.Errorf("%s cannot be cleared", field)
		}
		return nil, !equalPtr[uuid.UUID](nil, current), nil
	}
	parsed, err := uuid.Parse(*raw)
	if err != nil {
		return current, false, fmt.Errorf("invalid %s", field)
	}
	if parsed == uuid.Nil {
		return current, false, fmt.Errorf("%s cannot be the nil UUID", field)
	}
	return &parsed, !equalPtr(&parsed, current), nil
}

// equalPtr reports whether two optional values are equal, treating nil
// (absent/cleared) as distinct from any set value. Used for the tri-state PATCH
// fields (uuid.UUID and string), so nil-vs-set and set-vs-set both compare
// correctly.
func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func documentMetaResponse(doc model.Document) DocumentMetaResponse {
	return DocumentMetaResponse{
		ID:                doc.ID.String(),
		ExternalID:        doc.ExternalID,
		MimeType:          doc.MimeType,
		Size:              doc.Size,
		DisplayName:       doc.DisplayName,
		TemporaryLocation: doc.TemporaryLocation,
		StorageBucketID:   doc.StorageBucketID.String(),
		AuthorizationID:   optionalUUIDString(nonNilUUID(doc.AuthorizationID)),
		CreatedBy:         optionalUUIDString(doc.CreatedBy),
		TagsetID:          optionalUUIDString(doc.TagsetID),
		ExternalReference: doc.ExternalReference,
		CreatedDate:       doc.CreatedDate,
		UpdatedDate:       doc.UpdatedDate,
		ImageWidth:        doc.ImageWidth,
		ImageHeight:       doc.ImageHeight,
	}
}
