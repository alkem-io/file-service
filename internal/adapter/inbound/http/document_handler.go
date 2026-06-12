package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// GetMeta handles GET /internal/document/{id}/meta
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

// GetContent handles GET /internal/document/{id}/content
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

// buildCreateInput validates the collected metadata fields (spec 020: the
// fields trail the file part in the multipart body, so they are parsed
// after the upload has been staged — research R4).
func buildCreateInput(fields map[string]string) (input model.CreateDocumentInput, allowedMimeTypes []string, maxFileSize int, err error) {
	displayName := fields["displayName"]
	if err := validateDisplayName(displayName); err != nil {
		return input, nil, 0, err
	}

	storageBucketID, err := uuid.Parse(fields["storageBucketId"])
	if err != nil {
		return input, nil, 0, fmt.Errorf("invalid storageBucketId")
	}

	authorizationID, err := uuid.Parse(fields["authorizationId"])
	if err != nil {
		return input, nil, 0, fmt.Errorf("invalid authorizationId")
	}

	var tagsetID *uuid.UUID
	if v := fields["tagsetId"]; v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			return input, nil, 0, fmt.Errorf("invalid tagsetId")
		}
		tagsetID = &parsed
	}

	var createdBy *uuid.UUID
	if v := fields["createdBy"]; v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			return input, nil, 0, fmt.Errorf("invalid createdBy")
		}
		createdBy = &parsed
	}

	temporaryLocation := false
	if v := fields["temporaryLocation"]; v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return input, nil, 0, fmt.Errorf("invalid temporaryLocation: must be true or false")
		}
		temporaryLocation = parsed
	}

	skipDedup := false
	if v := fields["skipDedup"]; v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return input, nil, 0, fmt.Errorf("invalid skipDedup: must be true or false")
		}
		skipDedup = parsed
	}

	if v := fields["allowedMimeTypes"]; v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				allowedMimeTypes = append(allowedMimeTypes, trimmed)
			}
		}
	}

	if v := fields["maxFileSize"]; v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			return input, nil, 0, fmt.Errorf("invalid maxFileSize: must be a non-negative integer")
		}
		maxFileSize = parsed
	}

	input = model.CreateDocumentInput{
		DisplayName:       displayName,
		CreatedBy:         createdBy,
		TemporaryLocation: temporaryLocation,
		StorageBucketID:   storageBucketID,
		AuthorizationID:   authorizationID,
		TagsetID:          tagsetID,
		SkipDedup:         skipDedup,
	}
	return input, allowedMimeTypes, maxFileSize, nil
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
// whole-file buffering; metadata fields are collected after it (R4) and
// validated before the stage is published.
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

	fields := map[string]string{}
	var staged *service.StagedUpload
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
			declaredMIME := part.Header.Get("Content-Type")
			staged, err = h.Service.StageUpload(r.Context(), part, declaredMIME)
			if err != nil {
				h.writeStageError(w, err)
				return
			}
		} else {
			b, rerr := io.ReadAll(io.LimitReader(part, 64<<10))
			if rerr != nil {
				h.writeIngestTransportError(w, rerr)
				return
			}
			fields[part.FormName()] = string(b)
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
		return
	}

	IngestOutcomes.Add("accepted", 1)
	CreateDocumentResponse{
		ID:          doc.ID.String(),
		ExternalID:  doc.ExternalID,
		MimeType:    doc.MimeType,
		Size:        doc.Size,
		Reused:      doc.Reused,
		ImageWidth:  doc.ImageWidth,
		ImageHeight: doc.ImageHeight,
	}.Render(w)
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
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if dec.More() {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: trailing data after first object")
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

	var tagsetID *uuid.UUID
	if body.TagsetID != nil && *body.TagsetID != "" {
		parsed, err := uuid.Parse(*body.TagsetID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid tagsetId")
			return
		}
		tagsetID = &parsed
	}

	var createdBy *uuid.UUID
	if body.CreatedBy != nil && *body.CreatedBy != "" {
		parsed, err := uuid.Parse(*body.CreatedBy)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid createdBy")
			return
		}
		createdBy = &parsed
	}

	input := model.CopyDocumentInput{
		DestinationBucketID: destBucketID,
		AuthorizationID:     authID,
		TagsetID:            tagsetID,
		CreatedBy:           createdBy,
		SkipDedup:           body.SkipDedup,
	}

	doc, err := h.Service.CopyDocument(r.Context(), sourceID, input)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrDocumentNotFound):
			writeJSONError(w, http.StatusNotFound, "source document not found")
		case errors.Is(err, service.ErrConflict):
			// Reachable only when SkipDedup=true and the destination bucket
			// already has a row with this content under the unique index.
			writeJSONError(w, http.StatusConflict, "skipDedup requested but a row with this content already exists in the destination bucket")
		default:
			h.Logger.Error("failed to copy document", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	CreateDocumentResponse{
		ID:          doc.ID.String(),
		ExternalID:  doc.ExternalID,
		MimeType:    doc.MimeType,
		Size:        doc.Size,
		Reused:      doc.Reused,
		ImageWidth:  doc.ImageWidth,
		ImageHeight: doc.ImageHeight,
	}.Render(w)
}

// Delete handles DELETE /internal/document/{id}
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
// Mutates storageBucketId, temporaryLocation, and/or displayName. Each
// field is optional; at least one must be present. Omitted fields retain
// their current value.
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
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // immutable fields like mimeType must not silently no-op
	if err := dec.Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if dec.More() {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body: trailing data after first object")
		return
	}

	if body.StorageBucketID == nil && body.TemporaryLocation == nil && body.DisplayName == nil {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	if body.DisplayName != nil {
		if err := validateDisplayName(*body.DisplayName); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
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

	bucketID := doc.StorageBucketID
	if body.StorageBucketID != nil {
		parsed, err := uuid.Parse(*body.StorageBucketID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid storageBucketId")
			return
		}
		bucketID = parsed
	}

	tempLoc := doc.TemporaryLocation
	if body.TemporaryLocation != nil {
		tempLoc = *body.TemporaryLocation
	}

	displayName := doc.DisplayName
	if body.DisplayName != nil {
		displayName = *body.DisplayName
	}

	updated, err := h.Service.UpdateDocumentMetadata(r.Context(), docID, bucketID, tempLoc, displayName, doc.Version)
	if err != nil {
		if errors.Is(err, service.ErrConflict) {
			writeJSONError(w, http.StatusConflict, "document was modified concurrently, retry with fresh version")
			return
		}
		if errors.Is(err, model.ErrDuplicateKey) {
			writeJSONError(w, http.StatusConflict, "destination bucket already contains a document with this content")
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
