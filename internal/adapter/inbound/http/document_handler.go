package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/service"
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
		writeJSONError(w, http.StatusNotFound, "file not found on storage")
		return
	}

	w.Header().Set("Content-Type", doc.MimeType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

// Create handles POST /internal/document
func (h *DocumentHandler) Create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // 32MB limit
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing file part")
		return
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(file)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read file")
		return
	}

	displayName := r.FormValue("displayName")
	if displayName == "" {
		writeJSONError(w, http.StatusBadRequest, "displayName is required")
		return
	}

	storageBucketID, err := uuid.Parse(r.FormValue("storageBucketId"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid storageBucketId")
		return
	}

	authorizationID, err := uuid.Parse(r.FormValue("authorizationId"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid authorizationId")
		return
	}

	var tagsetID *uuid.UUID
	if v := r.FormValue("tagsetId"); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid tagsetId")
			return
		}
		tagsetID = &parsed
	}

	var createdBy *uuid.UUID
	if v := r.FormValue("createdBy"); v != "" {
		parsed, err := uuid.Parse(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid createdBy")
			return
		}
		createdBy = &parsed
	}

	temporaryLocation := r.FormValue("temporaryLocation") == "true"

	var allowedMimeTypes []string
	if v := r.FormValue("allowedMimeTypes"); v != "" {
		parts := strings.Split(v, ",")
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				allowedMimeTypes = append(allowedMimeTypes, trimmed)
			}
		}
	}

	maxFileSize := 0
	if v := r.FormValue("maxFileSize"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid maxFileSize")
			return
		}
		maxFileSize = parsed
	}

	input := model.CreateDocumentInput{
		DisplayName:       displayName,
		CreatedBy:         createdBy,
		TemporaryLocation: temporaryLocation,
		StorageBucketID:   storageBucketID,
		AuthorizationID:   authorizationID,
		TagsetID:          tagsetID,
	}

	doc, err := h.Service.CreateDocument(r.Context(), input, content, allowedMimeTypes, maxFileSize)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrPayloadTooLarge):
			writeJSONError(w, http.StatusRequestEntityTooLarge, "file too large")
		case errors.Is(err, service.ErrUnsupportedMediaType):
			writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported media type")
		default:
			h.Logger.Error("failed to create document", zap.Error(err))
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	CreateDocumentResponse{
		ID:         doc.ID.String(),
		ExternalID: doc.ExternalID,
		MimeType:   doc.MimeType,
		Size:       doc.Size,
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

// Update handles PATCH /internal/document/{id}
func (h *DocumentHandler) Update(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	var body UpdateDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Need at least one field
	if body.StorageBucketID == nil && body.TemporaryLocation == nil {
		writeJSONError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	// Get current doc to fill defaults
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

	updated, err := h.Service.UpdateDocumentLocation(r.Context(), docID, bucketID, tempLoc)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to update document", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	UpdateDocumentResponse{
		ID:                updated.ID.String(),
		StorageBucketID:   updated.StorageBucketID.String(),
		TemporaryLocation: updated.TemporaryLocation,
	}.Render(w)
}

// ReplaceContent handles PUT /internal/document/{id}/content (store-and-link)
func (h *DocumentHandler) ReplaceContent(w http.ResponseWriter, r *http.Request) {
	docID, err := parseDocID(r)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid document ID")
		return
	}

	content, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	result, err := h.Service.StoreAndLink(r.Context(), docID, content)
	if err != nil {
		if errors.Is(err, model.ErrDocumentNotFound) {
			writeJSONError(w, http.StatusNotFound, "document not found")
			return
		}
		h.Logger.Error("failed to replace content", zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	ReplaceContentResponse{
		ExternalID: result.ExternalID,
		MimeType:   result.MimeType,
		Size:       result.Size,
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
