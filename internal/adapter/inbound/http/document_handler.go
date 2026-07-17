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
	"os"
	"strconv"
	"strings"
	"time"

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

// GetMeta handles GET /internal/file/{id}/meta
func (h *DocumentHandler) GetMeta(w http.ResponseWriter, r *http.Request) {
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

	documentMetaResponse(doc).Render(w)
}

// ByReference handles GET /internal/file/by-reference?ref=<v>&bucketId=<uuid?>.
// ref is required. bucketId omitted → GLOBAL resolution (the provider's fetch:
// any document carrying the reference, all sharing one blob). bucketId present
// → bucket-SCOPED resolution (read resolution: the document in that bucket).
// 200 → document meta; 404 → no match.
func (h *DocumentHandler) ByReference(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeJSONError(w, http.StatusBadRequest, "missing required query parameter: ref")
		return
	}

	var (
		doc model.Document
		err error
	)
	if bucketParam := r.URL.Query().Get("bucketId"); bucketParam == "" {
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
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to lookup document by reference", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
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

	content, err := h.Service.Storage.Read(doc.ExternalID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "file not found on storage")
		} else {
			h.Logger.Error("failed to read file from storage", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	w.Header().Set("Content-Type", doc.MimeType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
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
}

func (f *createFields) set(name, value string) {
	switch name {
	case "displayName":
		f.displayName = value
	case "storageBucketId":
		f.storageBucketID = value
	case "authorizationId":
		f.authorizationID = value
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
	}
}

// parseOptionalUUID parses an optional UUID form field: empty means absent
// (nil, nil). The error names the field so it can serve directly as a 400
// body.
func parseOptionalUUID(value, field string) (*uuid.UUID, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", field)
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
func buildCreateInput(fields createFields) (input model.CreateDocumentInput, allowedMimeTypes []string, maxFileSize int, err error) {
	if err := validateDisplayName(fields.displayName); err != nil {
		return input, nil, 0, err
	}

	storageBucketID, err := uuid.Parse(fields.storageBucketID)
	if err != nil {
		return input, nil, 0, fmt.Errorf("invalid storageBucketId")
	}

	authorizationID, err := uuid.Parse(fields.authorizationID)
	if err != nil {
		return input, nil, 0, fmt.Errorf("invalid authorizationId")
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

	skipImageProcessing, err := parseOptionalBool(fields.skipImageProcessing, "skipImageProcessing")
	if err != nil {
		return input, nil, 0, err
	}

	if v := fields.allowedMimeTypes; v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				allowedMimeTypes = append(allowedMimeTypes, trimmed)
			}
		}
	}

	if v := fields.maxFileSize; v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			return input, nil, 0, fmt.Errorf("invalid maxFileSize: must be a non-negative integer")
		}
		maxFileSize = parsed
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
		SkipImageProcessing: skipImageProcessing,
		SkipDedup:           skipDedup,
	}
	return input, allowedMimeTypes, maxFileSize, nil
}

// optionalString maps an empty multipart field to nil and any non-empty value
// to a pointer — the wire representation of an absent optional string field.
func optionalString(v string) *string {
	if v == "" {
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

// validateExternalReference rejects a reference longer than the column allows.
// ref is the already-normalized optional value (nil = no reference).
func validateExternalReference(ref *string) error {
	if ref != nil && len(*ref) > maxExternalReferenceLen {
		return fmt.Errorf("externalReference exceeds maximum length of %d bytes", maxExternalReferenceLen)
	}
	return nil
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
	// contract (see the after-file guard below).
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
			// The verbatim (raw-store) decision is taken at stage time, so the
			// skipImageProcessing flag MUST precede the file part in the
			// multipart body (the provider sends metadata first). A
			// skipImageProcessing=true arriving AFTER the file is rejected
			// below rather than silently transcoded.
			skipImageProcessing, perr := parseOptionalBool(fields.skipImageProcessing, "skipImageProcessing")
			if perr != nil {
				writeJSONError(w, http.StatusBadRequest, perr.Error())
				return
			}
			stagedSkip = skipImageProcessing
			declaredMIME := part.Header.Get("Content-Type")
			staged, err = h.Service.StageUpload(r.Context(), part, declaredMIME, skipImageProcessing)
			if err != nil {
				h.writeStageError(w, err)
				return
			}
		} else {
			if !h.readMetadataField(w, &fields, part) {
				return
			}
			// Verbatim-store contract guard (spec 013): reject only when
			// skipImageProcessing=true is FIRST established after the file part
			// has already been staged — the bytes may already have been
			// transcoded/rotated, so the byte-exact contract cannot be honored.
			// A duplicate part consistent with the value staged before the file
			// (stagedSkip) is honored, so it must not 400.
			if staged != nil && part.FormName() == "skipImageProcessing" {
				skip, perr := parseOptionalBool(fields.skipImageProcessing, "skipImageProcessing")
				if perr != nil {
					writeJSONError(w, http.StatusBadRequest, perr.Error())
					return
				}
				if skip && !stagedSkip {
					writeJSONError(w, http.StatusBadRequest, "skipImageProcessing must be sent before the file part")
					return
				}
			}
		}
		_ = part.Close()
	}

	if staged == nil {
		writeJSONError(w, http.StatusBadRequest, "missing file part")
		return
	}

	input, allowedMimeTypes, maxFileSize, err := buildCreateInput(fields)
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
		writeJSONError(w, http.StatusBadRequest, part.FormName()+" exceeds the 16 KiB field limit")
		return false
	}
	fields.set(part.FormName(), string(b))
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
		writeJSONError(w, http.StatusConflict, "a conflicting document already exists (unique constraint)")
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
	if !decodeStrictJSON(w, r, &body) {
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
			// A unique-constraint collision in the destination bucket with no
			// reference winner. Arises with or without SkipDedup — e.g. a
			// SkipDedup=true content collision, or a normal reference-bearing
			// copy that collides on a different unique index.
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

	resp := DeleteDocumentResponse{
		AuthorizationID: deleted.AuthorizationID.String(),
	}
	if deleted.TagsetID != nil {
		s := deleted.TagsetID.String()
		resp.TagsetID = &s
	}
	resp.Render(w)
}

// Update handles PATCH /internal/file/{id}.
// Mutates the "move + re-attribute" fields: storageBucketId, temporaryLocation,
// displayName, authorizationId, createdBy, and externalReference. Each field is
// optional; at least one must produce an effective change. Omitted fields retain
// their current value; authorizationId, createdBy, and externalReference accept
// an explicit JSON null to clear.
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

	var body UpdateDocumentRequest
	present, ok := decodeUpdateRequest(w, r, &body)
	if !ok {
		return
	}

	if len(present) == 0 {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	if body.DisplayName != nil {
		if err := validateDisplayName(*body.DisplayName); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// externalReference is tri-state: absent = keep, explicit null = clear. An
	// empty *string value* is neither, so it is rejected rather than silently
	// mapped to NULL (clearing is done with an explicit null). This also keeps
	// PATCH from ever persisting a non-NULL empty reference.
	if _, ok := present["externalReference"]; ok && body.ExternalReference != nil && *body.ExternalReference == "" {
		writeJSONError(w, http.StatusBadRequest, "externalReference must not be empty")
		return
	}
	if err := validateExternalReference(body.ExternalReference); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
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

	meta, applied, err := buildMetadataUpdate(doc, body, present)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// No field carries an effective change — either every key is a no-op
	// explicit-null (e.g. {"temporaryLocation":null}) or sets a field to its
	// current value (e.g. {"displayName":"<current name>"}). Reject rather than
	// bump version+updatedDate on an unchanged row (which would spuriously 409 a
	// concurrent actor).
	if applied == 0 {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	updated, err := h.Service.UpdateDocumentMetadata(r.Context(), docID, meta, doc.Version)
	if err != nil {
		if errors.Is(err, service.ErrConflict) {
			writeJSONError(w, http.StatusConflict, "document was modified concurrently, retry with fresh version")
			return
		}
		if errors.Is(err, model.ErrDuplicateKey) {
			// PATCH never changes externalID, so the collision is not "same
			// content": with the (externalReference, storageBucketId) index a
			// move can collide on reference, and the authorizationId unique
			// constraint can collide on re-attribution. Keep the message
			// generic rather than naming the wrong cause.
			writeJSONError(w, http.StatusConflict, "update conflicts with an existing document in the destination bucket (duplicate reference, authorization, or content)")
			return
		}
		h.Logger.Error("failed to update document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Lazy-backfill: legacy image rows have empty content_metadata; PATCH
	// is one of the four metadata-returning endpoints that must surface
	// dims (FR-015 / FR-018). Best-effort — never fails the PATCH.
	updated = h.Service.BackfillIfNeeded(r.Context(), updated)

	UpdateDocumentResponse{
		ID:                updated.ID.String(),
		StorageBucketID:   updated.StorageBucketID.String(),
		TemporaryLocation: updated.TemporaryLocation,
		DisplayName:       updated.DisplayName,
		ImageWidth:        updated.ImageWidth,
		ImageHeight:       updated.ImageHeight,
	}.Render(w)
}

// validateDisplayName rejects renames that would corrupt the row or
// produce a filename consumers can't safely use. Shared by POST (create)
// and PATCH (update) so both paths apply the same rules.
//
// The 512-byte cap is intentionally tighter than file."displayName"
// VARCHAR(512), which is character-based in Postgres: capping at bytes
// guarantees any accepted value fits regardless of UTF-8 expansion.
func validateDisplayName(name string) error {
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
			writeJSONError(w, http.StatusConflict, "new content conflicts with another document in this bucket")
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

// decodeStrictJSON decodes the request body into dst, rejecting unknown
// fields and any trailing data after the first JSON object. It reports true
// on success; on failure it returns false AFTER writing the 400 response
// itself, so callers must simply return without touching w further.
//
// DisallowUnknownFields is load-bearing: immutable fields (e.g. mimeType on
// PATCH) must surface as a 400 rather than silently no-op.
func decodeStrictJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: trailing data after first object")
		return false
	}
	return true
}

// decodeUpdateRequest strict-decodes the PATCH body into dst and returns the
// set of top-level keys that were actually present, so the handler can tell
// "field omitted" (keep) from "field explicitly null" (clear) for the
// tri-state fields. On any malformed input it writes the 400 itself and
// reports ok=false. Mirrors decodeStrictJSON's unknown-field / trailing-data
// rejection.
func decodeUpdateRequest(w http.ResponseWriter, r *http.Request, dst *UpdateDocumentRequest) (present map[string]struct{}, ok bool) {
	// The PATCH body is a small JSON metadata patch; cap it so io.ReadAll can't
	// buffer unbounded input. 1 MiB is far above any legitimate patch.
	const maxPatchBodyBytes = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxPatchBodyBytes)
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
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return nil, false
	}
	present = make(map[string]struct{}, len(keys))
	for k := range keys {
		present[k] = struct{}{}
	}
	return present, true
}

// buildMetadataUpdate merges the PATCH fields over the row's current values to
// produce the full DocumentMetadataUpdate the move primitive persists. Fields
// present in the body win; omitted fields keep what the document already has.
// authorizationId/createdBy/externalReference are the re-attribute fields;
// they honor explicit JSON null as "clear". A malformed UUID yields an error
// whose message is safe as a 400 body.
//
// applied reports how many fields carry an EFFECTIVE change — a new value that
// differs from the row's current value — so the handler can 400 a no-op PATCH
// instead of bumping version+updatedDate on an unchanged row (which would
// spuriously 409 a concurrent actor). This rejects both the explicit-null no-op
// (e.g. {"temporaryLocation":null}, present but merges nothing) AND the
// same-value no-op (e.g. {"displayName":"<current name>"}). doc is the freshly
// loaded row, so the comparison needs no extra DB round-trip.
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
		parsed, err := uuid.Parse(*body.StorageBucketID)
		if err != nil {
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
	if _, ok := present["authorizationId"]; ok {
		var newVal *uuid.UUID // nil for explicit null → clear
		if body.AuthorizationID != nil {
			parsed, err := uuid.Parse(*body.AuthorizationID)
			if err != nil {
				return meta, applied, fmt.Errorf("invalid authorizationId")
			}
			newVal = &parsed
		}
		if !equalUUIDPtr(newVal, meta.AuthorizationID) {
			meta.AuthorizationID = newVal
			applied++
		}
	}
	if _, ok := present["createdBy"]; ok {
		var newVal *uuid.UUID // nil for explicit null → clear
		if body.CreatedBy != nil {
			parsed, err := uuid.Parse(*body.CreatedBy)
			if err != nil {
				return meta, applied, fmt.Errorf("invalid createdBy")
			}
			newVal = &parsed
		}
		if !equalUUIDPtr(newVal, meta.CreatedBy) {
			meta.CreatedBy = newVal
			applied++
		}
	}
	if _, ok := present["externalReference"]; ok {
		if !equalStringPtr(body.ExternalReference, meta.ExternalReference) {
			meta.ExternalReference = body.ExternalReference // value, or nil for explicit null → clear
			applied++
		}
	}

	return meta, applied, nil
}

// equalUUIDPtr reports whether two optional UUIDs are equal, treating nil
// (absent/cleared) as distinct from any set value.
func equalUUIDPtr(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equalStringPtr reports whether two optional strings are equal, treating nil
// (absent/cleared) as distinct from any set value.
func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func documentMetaResponse(doc model.Document) DocumentMetaResponse {
	resp := DocumentMetaResponse{
		ID:                doc.ID.String(),
		ExternalID:        doc.ExternalID,
		MimeType:          doc.MimeType,
		Size:              doc.Size,
		DisplayName:       doc.DisplayName,
		TemporaryLocation: doc.TemporaryLocation,
		StorageBucketID:   doc.StorageBucketID.String(),
		AuthorizationID:   doc.AuthorizationID.String(),
		ExternalReference: doc.ExternalReference,
		CreatedDate:       doc.CreatedDate,
		UpdatedDate:       doc.UpdatedDate,
	}
	if doc.CreatedBy != nil {
		s := doc.CreatedBy.String()
		resp.CreatedBy = &s
	}
	if doc.TagsetID != nil {
		s := doc.TagsetID.String()
		resp.TagsetID = &s
	}
	return resp
}
