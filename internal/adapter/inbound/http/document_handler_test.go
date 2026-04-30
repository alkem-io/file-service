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

// skipDedup=true on the multipart form must bypass the dedup lookup even when
// an existing row matches. Returns 201 (uniform POST) with reused=false.
func TestDocumentHandler_Create_SkipDedup_BypassesDedup(t *testing.T) {
	h, repo, _ := newDocHandler()

	bucket := uuid.New()
	repo.findDoc = &model.Document{
		ID:              uuid.New(),
		ExternalID:      "sha3-of-hello",
		MimeType:        "text/plain",
		StorageBucketID: bucket,
		AuthorizationID: uuid.New(),
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "placeholder.docx")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "placeholder.docx")
	_ = writer.WriteField("storageBucketId", bucket.String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("skipDedup", "true")
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
		t.Errorf("reused = %v, want false (skipDedup bypasses dedup)", resp["reused"])
	}
}

// HTTP-level coverage for the ErrConflict → 409 mapping in the Create handler.
// Reachable when SkipDedup=true and the schema enforces unique(externalID,
// storageBucketID); the service surfaces ErrConflict rather than
// masquerading as Reused=true.
func TestDocumentHandler_Create_SkipDedup_Conflict_Returns409(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.createErr = model.ErrDuplicateKey

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "placeholder.docx")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "placeholder.docx")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("skipDedup", "true")
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
}

// Malformed skipDedup value → 400.
func TestDocumentHandler_Create_SkipDedup_InvalidValue_400(t *testing.T) {
	h, _, _ := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("skipDedup", "yes-please") // invalid bool
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)

	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed skipDedup", rr.Code)
	}
}

// POST /internal/file/copy: happy path. Source exists; copy to a different
// bucket returns 201 with Reused=false and a fresh ID.
func TestDocumentHandler_Copy_Success(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:              sourceID,
		ExternalID:      "sha3-of-content",
		MimeType:        "image/png",
		Size:            42,
		DisplayName:     "banner.png",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}

	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            sourceID.String(),
		DestinationBucketID: uuid.New().String(),
		AuthorizationID:     uuid.New().String(),
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if reused, ok := resp["reused"].(bool); !ok || reused {
		t.Errorf("reused = %v, want false for fresh copy", resp["reused"])
	}
	if resp["externalID"] != "sha3-of-content" {
		t.Errorf("externalID = %v, want preserved from source", resp["externalID"])
	}
	if resp["mimeType"] != "image/png" {
		t.Errorf("mimeType = %v, want preserved from source", resp["mimeType"])
	}
}

// POST /internal/file/copy: dedup hit returns 201 + reused=true.
func TestDocumentHandler_Copy_DedupHit_ReturnsReusedTrue(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	bucketB := uuid.New()
	existingID := uuid.New()
	repo.doc = model.Document{
		ID:         sourceID,
		ExternalID: "sha3-of-content",
		MimeType:   "image/png",
		Size:       42,
	}
	repo.findDoc = &model.Document{
		ID:              existingID,
		ExternalID:      "sha3-of-content",
		StorageBucketID: bucketB,
		AuthorizationID: uuid.New(),
	}

	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            sourceID.String(),
		DestinationBucketID: bucketB.String(),
		AuthorizationID:     uuid.New().String(),
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if reused, ok := resp["reused"].(bool); !ok || !reused {
		t.Errorf("reused = %v, want true on dedup hit", resp["reused"])
	}
	if resp["id"] != existingID.String() {
		t.Errorf("id = %v, want existing %v", resp["id"], existingID.String())
	}
}

// POST /internal/file/copy: source not found → 404.
func TestDocumentHandler_Copy_SourceNotFound_404(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.err = model.ErrDocumentNotFound

	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            uuid.New().String(),
		DestinationBucketID: uuid.New().String(),
		AuthorizationID:     uuid.New().String(),
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// POST /internal/file/copy: invalid sourceId → 400.
func TestDocumentHandler_Copy_InvalidSourceID_400(t *testing.T) {
	h, _, _ := newDocHandler()

	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            "not-a-uuid",
		DestinationBucketID: uuid.New().String(),
		AuthorizationID:     uuid.New().String(),
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// POST /internal/file/copy: malformed JSON body → 400.
func TestDocumentHandler_Copy_MalformedJSON_400(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader("{not-json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// POST /internal/file/copy: skipDedup=true + unique-key violation → 409.
func TestDocumentHandler_Copy_SkipDedup_Conflict_Returns409(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:         sourceID,
		ExternalID: "sha3-of-content",
		MimeType:   "image/png",
		Size:       42,
	}
	repo.createErr = model.ErrDuplicateKey

	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            sourceID.String(),
		DestinationBucketID: uuid.New().String(),
		AuthorizationID:     uuid.New().String(),
		SkipDedup:           true,
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rr.Code, rr.Body.String())
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

// Anonymous request (no actorID in context) reaches the auth-evaluation
// service. The auth port decides the outcome based on the document's
// authorization policy. With a deny-by-default mock, anonymous gets 403
// — not 401 from the handler.
func TestPublicHandler_AnonymousRequest_DeniedByPolicy_403(t *testing.T) {
	policyID := uuid.New()
	auth := &mockAuth{} // Allowed: false, no error
	h := &PublicHandler{
		Repo:    &mockDocRepo{doc: model.Document{ID: uuid.New(), AuthorizationID: policyID}},
		Auth:    auth,
		Storage: &mockStorage{},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	// no actorID in context — anonymous
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (policy denies anonymous)", rr.Code)
	}
	if auth.calls != 1 {
		t.Fatalf("auth-eval called %d times, want exactly 1 (anonymous must reach the policy decision point)", auth.calls)
	}
	if auth.lastActorID != "" {
		t.Errorf("auth received actorID = %q, want empty (anonymous)", auth.lastActorID)
	}
	if auth.lastPrivilege != "read" {
		t.Errorf("auth received privilege = %q, want %q", auth.lastPrivilege, "read")
	}
	if auth.lastAuthPolicyID != policyID.String() {
		t.Errorf("auth received policy = %q, want %q", auth.lastAuthPolicyID, policyID.String())
	}
}

// Anonymous request hits a public-bucket document whose policy grants
// global-anonymous: read. Auth port allows; handler must serve the file.
func TestPublicHandler_AnonymousRequest_AllowedByPolicy_200(t *testing.T) {
	policyID := uuid.New()
	auth := &mockAuth{result: model.AuthResult{Allowed: true}}
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              uuid.New(),
			ExternalID:      "abc",
			MimeType:        "image/png",
			AuthorizationID: policyID,
		}},
		Auth:    auth,
		Storage: &mockStorage{data: []byte("png-bytes")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	// no actorID in context — anonymous
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anonymous read allowed)", rr.Code)
	}
	if rr.Body.String() != "png-bytes" {
		t.Errorf("body = %q, want file content", rr.Body.String())
	}
	if auth.calls != 1 {
		t.Fatalf("auth-eval called %d times, want exactly 1", auth.calls)
	}
	if auth.lastActorID != "" {
		t.Errorf("auth received actorID = %q, want empty (anonymous)", auth.lastActorID)
	}
	if auth.lastPrivilege != "read" {
		t.Errorf("auth received privilege = %q, want %q", auth.lastPrivilege, "read")
	}
	if auth.lastAuthPolicyID != policyID.String() {
		t.Errorf("auth received policy = %q, want %q", auth.lastAuthPolicyID, policyID.String())
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

// --- PATCH displayName ---

func TestDocumentHandler_Patch_DisplayName_Happy(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	bucket := uuid.New()
	repo.doc = model.Document{
		ID:                docID,
		StorageBucketID:   bucket,
		TemporaryLocation: false,
		DisplayName:       "old.txt", // genuine rename, not a no-op
		Version:           7,
	}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"displayName": "renamed.txt"}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 1 {
		t.Fatalf("UpdateMetadata calls = %d, want 1", repo.updateMetadataCalls)
	}
	if repo.lastUpdateDisplayName != "renamed.txt" {
		t.Errorf("displayName passed = %q, want %q", repo.lastUpdateDisplayName, "renamed.txt")
	}
	if repo.lastUpdateBucketID != bucket {
		t.Errorf("bucketID passed = %s, want %s (should fall through from current)", repo.lastUpdateBucketID, bucket)
	}
	if repo.lastUpdateTemporary {
		t.Errorf("temporary passed = %v, want false (should fall through)", repo.lastUpdateTemporary)
	}
	if repo.lastUpdateVersion != 7 {
		t.Errorf("version passed = %d, want 7 (current version for optimistic lock)", repo.lastUpdateVersion)
	}

	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.DisplayName != "renamed.txt" {
		t.Errorf("response displayName = %q, want %q", resp.DisplayName, "renamed.txt")
	}
}

func TestDocumentHandler_Patch_DisplayName_Idempotent(t *testing.T) {
	// Renaming to the same name twice: handler/service does not branch on
	// "no-op", but the row's version still advances (DB UPDATE always
	// runs). The contract is "stable result", not "no DB write".
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{
		ID:              docID,
		StorageBucketID: uuid.New(),
		DisplayName:     "stable.txt",
		Version:         3,
	}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"displayName": "stable.txt"}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("iteration %d: status = %d, want 200, body: %s", i, rr.Code, rr.Body.String())
		}
		if repo.lastUpdateDisplayName != "stable.txt" {
			t.Errorf("iteration %d: displayName = %q, want stable.txt", i, repo.lastUpdateDisplayName)
		}
	}
	if repo.updateMetadataCalls != 2 {
		t.Errorf("UpdateMetadata calls = %d, want 2", repo.updateMetadataCalls)
	}
}

func TestDocumentHandler_Patch_DisplayName_CombinedFields(t *testing.T) {
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	newBucket := uuid.New()
	repo.doc = model.Document{
		ID:                docID,
		StorageBucketID:   uuid.New(),
		TemporaryLocation: true,
		DisplayName:       "new.pdf",
		Version:           1,
	}

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"storageBucketId":"` + newBucket.String() + `","temporaryLocation":false,"displayName":"new.pdf"}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateBucketID != newBucket {
		t.Errorf("bucketID = %s, want %s", repo.lastUpdateBucketID, newBucket)
	}
	if repo.lastUpdateTemporary {
		t.Errorf("temporary = %v, want false", repo.lastUpdateTemporary)
	}
	if repo.lastUpdateDisplayName != "new.pdf" {
		t.Errorf("displayName = %q, want new.pdf", repo.lastUpdateDisplayName)
	}
}

func TestDocumentHandler_Patch_DisplayName_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", `{"displayName":""}`},
		{"whitespace_only", `{"displayName":"   "}`},
		{"too_long", `{"displayName":"` + strings.Repeat("a", 513) + `"}`},
		{"forward_slash", `{"displayName":"sub/dir.txt"}`},
		{"backslash", `{"displayName":"sub\\dir.txt"}`},
		{"control_char_null", `{"displayName":"a\u0000b"}`},
		{"control_char_newline", `{"displayName":"a\u000ab"}`},
		{"control_char_del", `{"displayName":"a\u007fb"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New(), DisplayName: "ok.txt"}

			r := chi.NewRouter()
			r.Patch("/internal/file/{id}", h.Update)

			req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
			if repo.updateMetadataCalls != 0 {
				t.Errorf("UpdateMetadata called %d times; should reject before service", repo.updateMetadataCalls)
			}
		})
	}
}

func TestDocumentHandler_Patch_DisplayName_LengthBoundary(t *testing.T) {
	// validateDisplayName caps at 512 bytes (intentionally tighter than
	// VARCHAR(512), which is character-based in Postgres). These cases
	// pin that the cap is byte-based so a future rune-based regression
	// fails loudly.
	cases := []struct {
		name     string
		display  string
		wantCode int
	}{
		{"ascii_512", strings.Repeat("a", 512), http.StatusOK},
		{"ascii_513", strings.Repeat("a", 513), http.StatusBadRequest},
		// "€" = 3 bytes in UTF-8. 170*3 + 2 = 512 bytes exactly.
		{"utf8_512_bytes_172_runes", strings.Repeat("€", 170) + "ab", http.StatusOK},
		// One more "€" pushes to 513 bytes (171*3 + 2 = 515, pick 170+1+rest).
		{"utf8_over_512_bytes", strings.Repeat("€", 171), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			docID := uuid.New()
			repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}

			r := chi.NewRouter()
			r.Patch("/internal/file/{id}", h.Update)

			body := `{"displayName":"` + tc.display + `"}`
			req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (display has %d bytes), body: %s", rr.Code, tc.wantCode, len(tc.display), rr.Body.String())
			}
			if tc.wantCode == http.StatusBadRequest && repo.updateMetadataCalls != 0 {
				t.Errorf("UpdateMetadata called %d times; should reject before service", repo.updateMetadataCalls)
			}
		})
	}
}

func TestDocumentHandler_Patch_RejectsUnknownFields(t *testing.T) {
	// Immutable fields like mimeType must produce a clear 400 instead of
	// silently no-op'ing when sent through PATCH.
	cases := []struct {
		name string
		body string
	}{
		{"immutable_mimeType", `{"displayName":"x.txt","mimeType":"text/plain"}`},
		{"immutable_externalID", `{"displayName":"x.txt","externalID":"abc"}`},
		{"immutable_size", `{"displayName":"x.txt","size":42}`},
		{"unknown_field", `{"displayName":"x.txt","foo":"bar"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New()}

			r := chi.NewRouter()
			r.Patch("/internal/file/{id}", h.Update)

			req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
			if repo.updateMetadataCalls != 0 {
				t.Errorf("UpdateMetadata called %d times; unknown-field reject must come before service", repo.updateMetadataCalls)
			}
		})
	}
}

func TestDocumentHandler_Create_RejectsInvalidDisplayName(t *testing.T) {
	// validateDisplayName must run on POST too, otherwise POST accepts
	// names that the handler later refuses to update via PATCH.
	cases := []struct {
		name        string
		displayName string
	}{
		{"whitespace_only", "   "},
		{"forward_slash", "sub/dir.txt"},
		{"backslash", `sub\dir.txt`},
		{"too_long", strings.Repeat("a", 513)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newDocHandler()

			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreateFormFile("file", "test.txt")
			_, _ = part.Write([]byte("hello"))
			_ = writer.WriteField("displayName", tc.displayName)
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
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDocumentHandler_Patch_DuplicateKey_409(t *testing.T) {
	// Defensive: if a UNIQUE constraint (e.g. (externalID, storageBucketId))
	// fires when the caller PATCHes a doc into a bucket that already
	// contains the same blob, the handler must surface 409, not 500.
	h, repo, _ := newDocHandler()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	repo.updateErr = model.ErrDuplicateKey

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	body := `{"storageBucketId":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
}
