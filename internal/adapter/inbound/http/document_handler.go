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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// DocumentHandler handles all internal document endpoints.
type DocumentHandler struct {
	Service *service.FileService
	MaxAge  int
	Logger  *zap.Logger
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

// parseCreateForm extracts and validates all fields from the multipart create request.
func parseCreateForm(r *http.Request) (content []byte, declaredMIME string, input model.CreateDocumentInput, allowedMimeTypes []string, maxFileSize int, err error) {
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, "", input, nil, 0, fmt.Errorf("missing file part")
	}
	defer func() { _ = file.Close() }()

	declaredMIME = header.Header.Get("Content-Type")

	content, err = io.ReadAll(file)
	if err != nil {
		return nil, "", input, nil, 0, fmt.Errorf("failed to read file")
	}

	displayName := r.FormValue("displayName")
	if err := validateDisplayName(displayName); err != nil {
		return nil, "", input, nil, 0, err
	}

	storageBucketID, err := uuid.Parse(r.FormValue("storageBucketId"))
	if err != nil {
		return nil, "", input, nil, 0, fmt.Errorf("invalid storageBucketId")
	}

	authorizationID, err := uuid.Parse(r.FormValue("authorizationId"))
	if err != nil {
		return nil, "", input, nil, 0, fmt.Errorf("invalid authorizationId")
	}

	var tagsetID *uuid.UUID
	if v := r.FormValue("tagsetId"); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			return nil, "", input, nil, 0, fmt.Errorf("invalid tagsetId")
		}
		tagsetID = &parsed
	}

	var createdBy *uuid.UUID
	if v := r.FormValue("createdBy"); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			return nil, "", input, nil, 0, fmt.Errorf("invalid createdBy")
		}
		createdBy = &parsed
	}

	temporaryLocation := false
	if v := r.FormValue("temporaryLocation"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, "", input, nil, 0, fmt.Errorf("invalid temporaryLocation: must be true or false")
		}
		temporaryLocation = parsed
	}

	skipDedup := false
	if v := r.FormValue("skipDedup"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil, "", input, nil, 0, fmt.Errorf("invalid skipDedup: must be true or false")
		}
		skipDedup = parsed
	}

	if v := r.FormValue("allowedMimeTypes"); v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				allowedMimeTypes = append(allowedMimeTypes, trimmed)
			}
		}
	}

	if v := r.FormValue("maxFileSize"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			return nil, "", input, nil, 0, fmt.Errorf("invalid maxFileSize: must be a non-negative integer")
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

	return content, declaredMIME, input, allowedMimeTypes, maxFileSize, nil
}

// Create handles POST /internal/document
func (h *DocumentHandler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // 32MB limit
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	content, declaredMIME, input, allowedMimeTypes, maxFileSize, err := parseCreateForm(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.Service.CreateDocument(r.Context(), input, content, declaredMIME, allowedMimeTypes, maxFileSize)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPayloadTooLarge):
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file too large")
		case errors.Is(err, service.ErrUnsupportedMediaType):
			writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported media type")
		case errors.Is(err, service.ErrImageProcessing):
			writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, service.ErrConflict):
			// Reachable only when SkipDedup=true and a row with this
			// (externalID, storageBucketID) already exists.
			writeJSONError(w, http.StatusConflict, "skipDedup requested but a row with this content already exists in the bucket")
		default:
			h.Logger.Error("failed to create document", zap.Error(err))
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

// ReplaceContent handles PUT /internal/document/{id}/content (store-and-link)
func (h *DocumentHandler) ReplaceContent(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // 32MB limit
	content, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// ReplaceContent note: if the new content's hash matches another file row in
	// the same bucket, the unique(externalID, storageBucketID) index is violated.
	// The service returns ErrConflict → 409 in that case. UpdateFile (PATCH) does
	// not touch externalID, so it cannot trigger this.
	result, err := h.Service.StoreAndLink(r.Context(), docID, content)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrDocumentNotFound):
			writeJSONError(w, http.StatusNotFound, "document not found")
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
