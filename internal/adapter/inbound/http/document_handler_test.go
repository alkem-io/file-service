package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/service"
)

func newDocHandler() (*DocumentHandler, *mockDocRepo, *mockStorage) {
	repo := &mockDocRepo{}
	storage := &mockStorage{}
	svc := &service.FileService{
		Repo:      repo,
		Auth:      &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage:   storage,
		Processor: &stubProcessor{},
	}
	return &DocumentHandler{Service: svc, MaxAge: 86400, Logger: zap.NewNop()}, repo, storage
}

type stubProcessor struct{}

func (p *stubProcessor) DetectMIME(_ []byte) string { return "application/octet-stream" }
func (p *stubProcessor) Process(content []byte, mimeType string) ([]byte, string, error) {
	return content, mimeType, nil
}

func TestDocumentHandler_GetMeta_Found(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{
		ID:              docID,
		ExternalID:      "abc",
		MimeType:        "text/plain",
		Size:            5,
		DisplayName:     "test.txt",
		AuthorizationID: uuid.New(),
		StorageBucketID: uuid.New(),
	}

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+docID.String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["displayName"] != "test.txt" {
		t.Errorf("displayName = %v", body["displayName"])
	}
}

func TestDocumentHandler_GetMeta_NotFound(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = model.ErrDocumentNotFound

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_GetContent_Found(t *testing.T) {
	h, repo, storage := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "abc", MimeType: "text/plain"}
	storage.data = []byte("file content")

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/content", h.GetContent)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+docID.String()+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("Content-Type = %q", rr.Header().Get("Content-Type"))
	}
	if rr.Body.String() != "file content" {
		t.Errorf("body = %q", rr.Body.String())
	}
}

func TestDocumentHandler_GetContent_NotFound(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = model.ErrDocumentNotFound

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/content", h.GetContent)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_GetContent_FileMissing(t *testing.T) {
	h, repo, storage := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), ExternalID: "missing", MimeType: "text/plain"}
	storage.err = os.ErrNotExist

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/content", h.GetContent)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_Create_Success(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["id"] == nil {
		t.Error("missing id in response")
	}
	if resp["externalID"] == nil {
		t.Error("missing externalID in response")
	}
}

func TestDocumentHandler_Create_Success_NotReused(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if reused, ok := resp["reused"].(bool); !ok || reused {
		t.Errorf("reused = %v, want false for new insert", resp["reused"])
	}
}

// Dedup hit returns 201 (uniform POST success) with reused=true so callers
// that only branch on 2xx vs 4xx work correctly across all generators.
func TestDocumentHandler_Create_Dedup_ReturnsReusedTrue(t *testing.T) {
	h, repo, _ := newDocHandler()

	existingID := uuid.New()
	existingAuth := uuid.New()
	bucket := uuid.New()
	repo.findDoc = &model.Document{
		ID:              existingID,
		ExternalID:      "sha3-of-hello",
		MimeType:        "text/plain",
		Size:            5,
		StorageBucketID: bucket,
		AuthorizationID: existingAuth,
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", bucket.String())
	_ = writer.WriteField("authorizationId", uuid.New().String()) // caller-supplied, should be ignored
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (uniform POST success), body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if reused, ok := resp["reused"].(bool); !ok || !reused {
		t.Errorf("reused = %v, want true", resp["reused"])
	}
	if resp["id"] != existingID.String() {
		t.Errorf("id = %v, want existing %v", resp["id"], existingID.String())
	}
}

func TestDocumentHandler_Create_AllFields(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("tagsetId", uuid.New().String())
	_ = writer.WriteField("createdBy", uuid.New().String())
	_ = writer.WriteField("temporaryLocation", "true")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Create_InvalidAuthorizationId(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", "not-a-uuid")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Create_InvalidTagsetId(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("tagsetId", "bad")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Create_InvalidCreatedBy(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("createdBy", "bad")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Create_ServiceError(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.createErr = errors.New("FK constraint")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Patch_WithBucketId(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	newBucket := uuid.New()
	repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), TemporaryLocation: true}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"storageBucketId": "` + newBucket.String() + `", "temporaryLocation": false}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Delete_WithTagset(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	authID := uuid.New()
	tagsetID := uuid.New()
	tagsetStr := tagsetID.String()
	repo.doc = model.Document{ID: docID, ExternalID: "abc"}
	repo.deleteResult = model.DeletedDocument{AuthorizationID: authID, TagsetID: &tagsetID}
	repo.count = 1

	r := chi.NewRouter()
	r.Delete("/internal/file/{id}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/"+docID.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["tagsetId"] != tagsetStr {
		t.Errorf("tagsetId = %v, want %v", resp["tagsetId"], tagsetStr)
	}
}

func TestDocumentHandler_Delete_Success(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	authID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "abc"}
	repo.deleteResult = model.DeletedDocument{AuthorizationID: authID}
	repo.count = 1

	r := chi.NewRouter()
	r.Delete("/internal/file/{id}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/"+docID.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Patch_Success(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), TemporaryLocation: true}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"temporaryLocation": false}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_ReplaceContent_Success(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old"}

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", strings.NewReader("new content"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

// --- Create error paths ---

func TestDocumentHandler_Create_MissingFile(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	// Empty body, no multipart
	req := httptest.NewRequest(http.MethodPost, "/internal/file", strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Create_MissingDisplayName(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	// no displayName
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Create_InvalidStorageBucketId(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", "not-a-uuid")
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Create_TooLarge(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "big.bin")
	_, _ = part.Write([]byte("this is more than 5 bytes"))
	_ = writer.WriteField("displayName", "big.bin")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("maxFileSize", "5")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Create_UnsupportedMIME(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.bin")
	_, _ = part.Write([]byte("binary content"))
	_ = writer.WriteField("displayName", "test.bin")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("allowedMimeTypes", "image/jpeg,image/png")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415, body: %s", rr.Code, rr.Body.String())
	}
}

// --- Delete error paths ---

func TestDocumentHandler_Delete_NotFound(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.deleteErr = model.ErrDocumentNotFound

	r := chi.NewRouter()
	r.Delete("/internal/file/{id}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/"+uuid.New().String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_Delete_InvalidID(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Delete("/internal/file/{id}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// --- Update error paths ---

func TestDocumentHandler_Patch_NotFound(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = model.ErrDocumentNotFound

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"temporaryLocation": false}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_Patch_InvalidJSON(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader("{invalid"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDocumentHandler_Patch_EmptyBody(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for no fields", rr.Code)
	}
}

func TestDocumentHandler_Patch_InvalidBucketId(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New()}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"storageBucketId": "not-a-uuid"}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// --- GetMeta/GetContent with invalid ID ---

func TestDocumentHandler_GetMeta_InvalidID(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/not-a-uuid/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_GetContent_InvalidID(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/content", h.GetContent)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/not-a-uuid/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDocumentHandler_ReplaceContent_InvalidID(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/not-a-uuid/content", strings.NewReader("content"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// --- DocumentMetaResponse with nullable fields ---

func TestDocumentMetaResponse_WithNullables(t *testing.T) {
	createdBy := uuid.New()
	tagsetID := uuid.New()
	doc := model.Document{
		ID:              uuid.New(),
		ExternalID:      "hash",
		MimeType:        "text/plain",
		DisplayName:     "test.txt",
		CreatedBy:       &createdBy,
		TagsetID:        &tagsetID,
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}
	resp := documentMetaResponse(doc)
	if resp.CreatedBy == nil {
		t.Error("expected createdBy in response")
	}
	if resp.TagsetID == nil {
		t.Error("expected tagsetId in response")
	}
}

func TestDocumentMetaResponse_WithoutNullables(t *testing.T) {
	doc := model.Document{
		ID:              uuid.New(),
		ExternalID:      "hash",
		MimeType:        "text/plain",
		DisplayName:     "test.txt",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}
	resp := documentMetaResponse(doc)
	if resp.CreatedBy != nil {
		t.Error("createdBy should be nil when not set")
	}
	if resp.TagsetID != nil {
		t.Error("tagsetId should be nil when not set")
	}
}

func TestDocumentHandler_ReplaceContent_NotFound(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = model.ErrDocumentNotFound

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+uuid.New().String()+"/content", strings.NewReader("content"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// 409 path: new content's hash collides with another file row already in the same
// bucket. The unique(externalID, storageBucketID) index rejects the UPDATE.
// Auto-merging would lose distinct document identity, so we surface 409.
func TestDocumentHandler_ReplaceContent_ContentCollision_Returns409(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), ExternalID: "old-hash", StorageBucketID: uuid.New()}
	repo.updateErr = model.ErrDuplicateKey

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+uuid.New().String()+"/content", strings.NewReader("new content"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 on bucket content collision, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_ReplaceContent_InternalError(t *testing.T) {
	h, repo, storage := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), ExternalID: "old"}
	storage.saveErr = errors.New("disk full")

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+uuid.New().String()+"/content", strings.NewReader("content"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestDocumentHandler_Delete_InternalError(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), ExternalID: "abc"}
	repo.deleteErr = errors.New("db error")

	r := chi.NewRouter()
	r.Delete("/internal/file/{id}", h.Delete)

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/"+uuid.New().String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestDocumentHandler_Patch_UpdateError(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New()}
	repo.updateErr = errors.New("db error")

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"temporaryLocation": false}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for DB error", rr.Code)
	}
}

func TestDocumentHandler_Patch_VersionConflict(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New(), Version: 5}
	repo.updateErr = model.ErrDocumentNotFound // 0 rows = version mismatch

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"temporaryLocation": false}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 Conflict", rr.Code)
	}
}

func TestDocumentHandler_GetMeta_InternalError(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = errors.New("db connection lost")

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestDocumentHandler_GetContent_InternalError(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = errors.New("db connection lost")

	r := chi.NewRouter()
	r.Get("/internal/file/{id}/content", h.GetContent)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestPublicHandler_InvalidUUID(t *testing.T) {
	h := &PublicHandler{
		Repo:    &mockDocRepo{},
		Auth:    &mockAuth{},
		Storage: &mockStorage{},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/not-a-uuid", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for invalid UUID", rr.Code)
	}
}

func TestPublicHandler_MissingActorID(t *testing.T) {
	h := &PublicHandler{
		Repo:    &mockDocRepo{doc: model.Document{ID: uuid.New()}},
		Auth:    &mockAuth{},
		Storage: &mockStorage{},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	// no actorID in context
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for missing actorID", rr.Code)
	}
}

func TestPublicHandler_AuthServiceUnavailable(t *testing.T) {
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              uuid.New(),
			AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{err: errors.New("NATS timeout")},
		Storage: &mockStorage{},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for auth unavailable", rr.Code)
	}
}

func TestPublicHandler_DBInternalError(t *testing.T) {
	h := &PublicHandler{
		Repo:    &mockDocRepo{err: errors.New("connection reset")},
		Auth:    &mockAuth{},
		Storage: &mockStorage{},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for DB error", rr.Code)
	}
}

func TestInitMetrics(_ *testing.T) {
	// Just verify it doesn't panic
	InitMetrics()
	InitMetrics() // idempotent
}
