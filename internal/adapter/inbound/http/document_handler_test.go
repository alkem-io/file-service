package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

func newDocHandler() (*DocumentHandler, *mockDocRepo, *mockStorage) {
	h, repo, storage, _ := newDocHandlerWithProcessor()
	return h, repo, storage
}

// newDocHandlerWithProcessor returns the same handler/repo/storage trio as
// newDocHandler plus the stubProcessor, so US1 tests can override its
// per-test fields (Process dim returns, MeasureDims behavior, declared
// MIME) before issuing the request.
func newDocHandlerWithProcessor() (*DocumentHandler, *mockDocRepo, *mockStorage, *stubProcessor) {
	repo := &mockDocRepo{}
	storage := &mockStorage{}
	processor := &stubProcessor{}
	svc := &service.FileService{
		Repo:      repo,
		Auth:      &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage:   storage,
		Processor: processor,
		Logger:    zap.NewNop(),
	}
	return &DocumentHandler{Service: svc, MaxAge: 86400, Logger: zap.NewNop()}, repo, storage, processor
}

func intp(v int) *int { return &v }

type stubProcessor struct {
	// Per-test overrides for Process result dims. Measured is derived from
	// whether dims are set: tests that pin dims model the "decoder ran and
	// produced dims" path; tests that leave them nil model the "no decoder
	// available" path (Measured=false). Tests that need to exercise the
	// "decoder ran but failed" path (Measured=true with nil dims, writing
	// the _decodeFailed sentinel) must opt in via processMeasured.
	processDimsW    *int
	processDimsH    *int
	processMeasured bool // when true, forces Measured=true regardless of dims

	// MeasureDims override (lazy-backfill tests).
	measureDimsW   *int
	measureDimsH   *int
	measureDimsErr error

	// detectMIME override — when non-empty, replaces the default
	// application/octet-stream so image-MIME tests exercise the
	// dims-on-response paths.
	detectMIME string

	// transcodeCalls counts TranscodeStream invocations, so ordering tests can
	// prove the verbatim arm ran (0) vs the transcode arm (>0).
	transcodeCalls int
}

func (p *stubProcessor) DetectMIME(_ []byte) string {
	if p.detectMIME != "" {
		return p.detectMIME
	}
	return "application/octet-stream"
}
func (p *stubProcessor) Process(content []byte, mimeType string) (port.ProcessResult, error) {
	// Mirror the marshalContentMetadata invariant: dims must be both set or both nil.
	// Fail fast at the source so test setup bugs surface immediately.
	if (p.processDimsW == nil) != (p.processDimsH == nil) {
		return port.ProcessResult{}, fmt.Errorf("stubProcessor: inconsistent dims: width=%v height=%v", p.processDimsW, p.processDimsH)
	}
	measured := p.processMeasured || (p.processDimsW != nil && p.processDimsH != nil)
	return port.ProcessResult{
		Content:     content,
		MimeType:    mimeType,
		ImageWidth:  p.processDimsW,
		ImageHeight: p.processDimsH,
		Measured:    measured,
	}, nil
}
func (p *stubProcessor) MeasureDims(_ []byte, _ string) (*int, *int, error) {
	return p.measureDimsW, p.measureDimsH, p.measureDimsErr
}

// runPatch wires a fresh handler + chi router and dispatches a single
// PATCH /internal/file/{id} request with the given JSON body. The
// configure callback (if non-nil) lets the caller seed mock state on
// the repo before the request is served. Returns the response recorder
// and the captured repo so callers can assert status + UpdateMetadata
// args without repeating chi/httptest boilerplate per test.
func runPatch(t *testing.T, docID uuid.UUID, body string, configure func(*mockDocRepo)) (*httptest.ResponseRecorder, *mockDocRepo) {
	t.Helper()
	h, repo, _ := newDocHandler()
	if configure != nil {
		configure(repo)
	}
	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr, repo
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

	body := `{"temporaryLocation": true}` // effective change (fixture defaults false) so the update path runs
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

	body := `{"temporaryLocation": true}` // effective change (fixture defaults false) so the update path runs
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

func TestDocumentHandler_Patch_DisplayName_SameValueNoOp_Idempotent200(t *testing.T) {
	// Renaming to the CURRENT name carries no effective change: an idempotent
	// PATCH must return 200 with the current document and write NOTHING — no
	// version+updatedDate bump on an unchanged row (which would spuriously 409 a
	// concurrent actor) — while still succeeding for idempotent/retry callers.
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
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for same-value no-op; body=%s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("UpdateMetadata calls = %d, want 0 (no write on same-value no-op)", repo.updateMetadataCalls)
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != docID.String() {
		t.Errorf("id = %q, want %q (current document echoed back)", resp.ID, docID.String())
	}
	if resp.DisplayName != "stable.txt" {
		t.Errorf("displayName = %q, want %q (unchanged current value)", resp.DisplayName, "stable.txt")
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
		DisplayName:       "old.pdf", // fixture differs from body so a missing rename fails the test
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
			rr, repo := runPatch(t, uuid.New(), tc.body, func(repo *mockDocRepo) {
				repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New(), DisplayName: "ok.txt"}
			})
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
		// 171 * 3 = 513 bytes, just over the limit.
		{"utf8_over_512_bytes", strings.Repeat("€", 171), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docID := uuid.New()
			rr, repo := runPatch(t, docID, `{"displayName":"`+tc.display+`"}`, func(repo *mockDocRepo) {
				repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
			})
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
			rr, repo := runPatch(t, uuid.New(), tc.body, func(repo *mockDocRepo) {
				repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New()}
			})
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
		{"control_char_null", "a\x00b"},
		{"control_char_newline", "a\nb"},
		{"control_char_del", "a\x7fb"},
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
	// Asserting the body too pins the duplicate-key vs version-conflict
	// distinction so callers can act on it without parsing structured
	// error codes.
	docID := uuid.New()
	rr, _ := runPatch(t, docID, `{"storageBucketId":"`+uuid.New().String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
		repo.updateErr = model.ErrDuplicateKey
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for duplicate key, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "update conflicts with an existing document") {
		t.Errorf("conflict body = %q, want duplicate-key message (not version-conflict)", rr.Body.String())
	}
}

func TestDocumentHandler_Patch_RejectsTrailingJSON(t *testing.T) {
	// json.Decoder.Decode consumes only the first JSON value, so a body
	// like `{"displayName":"x.txt"}{"foo":1}` would otherwise parse the
	// first object and silently drop the second. Reject it instead.
	rr, repo := runPatch(t, uuid.New(), `{"displayName":"x.txt"}{"foo":1}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: uuid.New(), StorageBucketID: uuid.New()}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("UpdateMetadata called %d times; trailing JSON must be rejected before service", repo.updateMetadataCalls)
	}
}

func TestDocumentHandler_Copy_RejectsTrailingJSON(t *testing.T) {
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	body := `{"sourceId":"` + uuid.New().String() + `","destinationBucketId":"` + uuid.New().String() + `","authorizationId":"` + uuid.New().String() + `"}{"extra":1}`
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
}

func TestDocumentHandler_Copy_RejectsUnknownFields(t *testing.T) {
	// Tightening the Copy decoder along with the trailing-data check
	// also catches caller typos and immutable-field attempts.
	h, _, _ := newDocHandler()

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)

	body := `{"sourceId":"` + uuid.New().String() + `","destinationBucketId":"` + uuid.New().String() + `","authorizationId":"` + uuid.New().String() + `","mimeType":"text/plain"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
}

// --- US1: dim reporting on Create / ReplaceContent responses ---

// TestDocumentHandler_Create_Image_ReturnsDims defends FR-014: a freshly-
// uploaded image must surface imageWidth/imageHeight on the
// CreateDocumentResponse.
func TestDocumentHandler_Create_Image_ReturnsDims(t *testing.T) {
	h, _, _, processor := newDocHandlerWithProcessor()
	processor.detectMIME = "image/jpeg"
	processor.processDimsW = intp(800)
	processor.processDimsH = intp(600)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "photo.jpg")
	_, _ = part.Write([]byte("\xFF\xD8\xFF\xE0jpeg-bytes"))
	_ = writer.WriteField("displayName", "photo.jpg")
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
	var resp CreateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 800 {
		t.Errorf("imageWidth = %v, want 800", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 600 {
		t.Errorf("imageHeight = %v, want 600", resp.ImageHeight)
	}
}

// TestDocumentHandler_Create_PhonePhoto_Orient6_ReportsRotatedDims is the
// SC-001 regression repro. Mock processor reports post-rotation dims
// (127×1082) for input that would have raw dims 1082×127 + orientation 6.
// Asserts the CreateDocumentResponse carries the rotated values.
func TestDocumentHandler_Create_PhonePhoto_Orient6_ReportsRotatedDims(t *testing.T) {
	h, _, _, processor := newDocHandlerWithProcessor()
	processor.detectMIME = "image/jpeg"
	processor.processDimsW = intp(127)  // post-rotation width
	processor.processDimsH = intp(1082) // post-rotation height

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "phone.jpg")
	_, _ = part.Write([]byte("phone-photo-bytes"))
	_ = writer.WriteField("displayName", "phone.jpg")
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
	var resp CreateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 127 {
		t.Errorf("imageWidth = %v, want 127 (post-rotation)", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 1082 {
		t.Errorf("imageHeight = %v, want 1082 (post-rotation)", resp.ImageHeight)
	}
}

// TestDocumentHandler_Create_RotatedPNG_DedupHitsOnReupload defends
// SC-005 / FR-010 on the new PNG canonicalization path: the same bytes
// uploaded twice must dedup on the second upload (reused=true). The
// canonical processing is deterministic, so the externalID hash is
// stable across re-uploads.
func TestDocumentHandler_Create_RotatedPNG_DedupHitsOnReupload(t *testing.T) {
	h, repo, _, processor := newDocHandlerWithProcessor()
	processor.detectMIME = "image/png"
	processor.processDimsW = intp(512)
	processor.processDimsH = intp(1024)

	bucket := uuid.New()
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake-but-mock-hashes-deterministically")
	postPNG := func() *httptest.ResponseRecorder {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("file", "rotated.png")
		_, _ = part.Write(pngBytes)
		_ = writer.WriteField("displayName", "rotated.png")
		_ = writer.WriteField("storageBucketId", bucket.String())
		_ = writer.WriteField("authorizationId", uuid.New().String())
		_ = writer.Close()

		r := chi.NewRouter()
		r.Post("/internal/file", h.Create)
		req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	// First upload: fresh insert, reused=false. Capture the externalID.
	rr1 := postPNG()
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first upload: status = %d, want 201, body: %s", rr1.Code, rr1.Body.String())
	}
	var resp1 CreateDocumentResponse
	if err := json.Unmarshal(rr1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal #1: %v", err)
	}
	if resp1.Reused {
		t.Errorf("first upload Reused = true, want false")
	}

	// Wire up the dedup hit for the second request: mockRepo.findDoc
	// returning a row with the same externalID + bucket. ContentMetadata
	// Populated=true so the lazy-backfill skips it (already measured).
	repo.findDoc = &model.Document{
		ID:              uuid.MustParse(resp1.ID),
		ExternalID:      resp1.ExternalID,
		MimeType:        "image/png",
		Size:            resp1.Size,
		StorageBucketID: bucket,
		AuthorizationID: uuid.New(),
		ContentMetadata: model.ContentMetadata{Populated: true, ImageWidth: intp(512), ImageHeight: intp(1024)},
		ImageWidth:      intp(512),
		ImageHeight:     intp(1024),
	}

	rr2 := postPNG()
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status = %d, want 201, body: %s", rr2.Code, rr2.Body.String())
	}
	var resp2 CreateDocumentResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal #2: %v", err)
	}
	if !resp2.Reused {
		t.Errorf("second upload Reused = false, want true (dedup hit)")
	}
	if resp2.ExternalID != resp1.ExternalID {
		t.Errorf("externalID mismatch on dedup: first=%q, second=%q", resp1.ExternalID, resp2.ExternalID)
	}
	// FR-004: every metadata-returning response carries dims, including the dedup-hit branch.
	if resp2.ImageWidth == nil || *resp2.ImageWidth != 512 {
		t.Errorf("dedup-hit response imageWidth = %v, want 512", resp2.ImageWidth)
	}
	if resp2.ImageHeight == nil || *resp2.ImageHeight != 1024 {
		t.Errorf("dedup-hit response imageHeight = %v, want 1024", resp2.ImageHeight)
	}
}

// --- Phase 7: lazy-backfill on Copy and PATCH for legacy image rows ---

// copyLegacyImageRow shared helper: prepares a handler with a legacy
// image source row (no dims) and a storage reading PNG-ish bytes whose
// MeasureDims returns the supplied (w, h, err). Returns the recorder and
// captured repo so callers can assert.
func runCopyLegacyImage(
	t *testing.T,
	measureW *int, measureH *int, measureErr error,
	storageData []byte, storageErr error,
	backfillErr error,
) (*httptest.ResponseRecorder, *mockDocRepo) {
	t.Helper()
	h, repo, storage, processor := newDocHandlerWithProcessor()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:              sourceID,
		ExternalID:      "sha3-of-png",
		MimeType:        "image/png",
		Size:            42,
		DisplayName:     "legacy.png",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		// dims nil → legacy
	}
	storage.data = storageData
	storage.err = storageErr
	processor.measureDimsW = measureW
	processor.measureDimsH = measureH
	processor.measureDimsErr = measureErr
	repo.backfillErr = backfillErr

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
	return rr, repo
}

// runPatchLegacyImage mirrors runCopyLegacyImage for the PATCH path.
func runPatchLegacyImage(
	t *testing.T,
	measureW *int, measureH *int, measureErr error,
	storageData []byte, storageErr error,
	backfillErr error,
) (*httptest.ResponseRecorder, *mockDocRepo) {
	t.Helper()
	h, repo, storage, processor := newDocHandlerWithProcessor()
	docID := uuid.New()
	repo.doc = model.Document{
		ID:              docID,
		ExternalID:      "sha3-of-png",
		MimeType:        "image/png",
		Size:            42,
		DisplayName:     "legacy.png",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}
	storage.data = storageData
	storage.err = storageErr
	processor.measureDimsW = measureW
	processor.measureDimsH = measureH
	processor.measureDimsErr = measureErr
	repo.backfillErr = backfillErr

	r := chi.NewRouter()
	r.Patch("/internal/file/{id}", h.Update)
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(`{"displayName":"renamed.png"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr, repo
}

func TestDocumentHandler_Copy_LegacyImageRow_LazyBackfillsBoth(t *testing.T) {
	rr, repo := runCopyLegacyImage(t, intp(800), intp(600), nil, []byte("png-bytes"), nil, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	var resp CreateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 800 {
		t.Errorf("imageWidth = %v, want 800", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 600 {
		t.Errorf("imageHeight = %v, want 600", resp.ImageHeight)
	}
	if repo.backfillCalls < 1 {
		t.Errorf("backfill calls = %d, want >= 1 (source row)", repo.backfillCalls)
	}
	// Persisted payload should be the dims, not the sentinel.
	got := repo.lastBackfillPayload
	if !got.Populated || got.DecodeFailed || got.ImageWidth == nil || got.ImageHeight == nil {
		t.Errorf("backfill payload = %+v, expected Populated=true with dims", got)
	}
}

func TestDocumentHandler_Patch_LegacyImageRow_LazyBackfills(t *testing.T) {
	rr, repo := runPatchLegacyImage(t, intp(640), intp(480), nil, []byte("png-bytes"), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 640 {
		t.Errorf("imageWidth = %v, want 640", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 480 {
		t.Errorf("imageHeight = %v, want 480", resp.ImageHeight)
	}
	if repo.backfillCalls < 1 {
		t.Errorf("backfill calls = %d, want >= 1", repo.backfillCalls)
	}
	got := repo.lastBackfillPayload
	if !got.Populated || got.DecodeFailed || got.ImageWidth == nil || got.ImageHeight == nil {
		t.Errorf("backfill payload = %+v, expected Populated=true with dims", got)
	}
}

// SC-010: MeasureDims returns an error → response 200, dims absent,
// content_metadata persisted as the {_decodeFailed: true} sentinel.
func TestDocumentHandler_Patch_DecodeFailure_PersistsSentinel(t *testing.T) {
	rr, repo := runPatchLegacyImage(t, nil, nil, errors.New("vips load: corrupt header"), []byte("corrupt-bytes"), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth != nil || resp.ImageHeight != nil {
		t.Errorf("expected nil dims on decode failure, got (%v, %v)", resp.ImageWidth, resp.ImageHeight)
	}
	if repo.backfillCalls < 1 {
		t.Errorf("backfill not called; sentinel must be persisted")
	}
	got := repo.lastBackfillPayload
	if !got.Populated || !got.DecodeFailed {
		t.Errorf("backfill payload = %+v, expected Populated=true with DecodeFailed=true", got)
	}
}

// SC-012 / FR-020 case b: Storage.Read fails → 200, no dims, no persist.
func TestDocumentHandler_Patch_StorageReadFails_GracefulDegrade(t *testing.T) {
	rr, repo := runPatchLegacyImage(t, intp(800), intp(600), nil, nil, errors.New("transient I/O"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth != nil || resp.ImageHeight != nil {
		t.Errorf("expected nil dims after Storage.Read failure, got (%v, %v)", resp.ImageWidth, resp.ImageHeight)
	}
	if repo.backfillCalls != 0 {
		t.Errorf("backfill calls = %d, want 0 (no sentinel on transient failures, FR-020 b)", repo.backfillCalls)
	}
}

// SC-013 / FR-020 case c: BackfillContentMetadata persist fails →
// response still carries the just-computed dims; row stays empty so the
// next read retries.
func TestDocumentHandler_Patch_BackfillPersistFails_ResponseStillCarriesDims(t *testing.T) {
	rr, repo := runPatchLegacyImage(t, intp(800), intp(600), nil, []byte("png-bytes"), nil, errors.New("DB transient"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 800 {
		t.Errorf("imageWidth = %v, want 800 (response carries dims even though persist failed)", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 600 {
		t.Errorf("imageHeight = %v, want 600", resp.ImageHeight)
	}
	if repo.backfillCalls < 1 {
		t.Errorf("backfill must still be attempted before failing")
	}
}

// TestDocumentHandler_ReplaceContent_RotatedImage_ReturnsDims defends FR-004
// for the Replace endpoint. PUT new image bytes; response carries the
// post-rotation dims sourced from StoredFile.ImageWidth/Height (which the
// service plumbs from ProcessResult inside StoreAndLink).
func TestDocumentHandler_ReplaceContent_RotatedImage_ReturnsDims(t *testing.T) {
	h, repo, _, processor := newDocHandlerWithProcessor()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old-hash", MimeType: "image/jpeg"}
	processor.detectMIME = "image/jpeg"
	processor.processDimsW = intp(640)
	processor.processDimsH = intp(480)

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", strings.NewReader("new image bytes"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp ReplaceContentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ImageWidth == nil || *resp.ImageWidth != 640 {
		t.Errorf("imageWidth = %v, want 640", resp.ImageWidth)
	}
	if resp.ImageHeight == nil || *resp.ImageHeight != 480 {
		t.Errorf("imageHeight = %v, want 480", resp.ImageHeight)
	}
}

// --- Spec 019: replace-path MIME protection ---

const testPptxMime = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
const testDocxMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// US1/T010: a generic sniff over an office document returns 200 and the
// response reports the unchanged stored type.
func TestDocumentHandler_ReplaceContent_KeepsStoredTypeOnGenericSniff(t *testing.T) {
	h, repo, _, processor := newDocHandlerWithProcessor()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old", MimeType: testPptxMime}
	processor.detectMIME = "application/zip"

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)
	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", strings.NewReader("collabora save-back bytes"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp ReplaceContentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MimeType != testPptxMime {
		t.Errorf("mimeType = %q, want %q (stored type must survive a generic sniff)", resp.MimeType, testPptxMime)
	}
}

// US2/T014: empty body → 422 EMPTY_CONTENT, row untouched.
func TestDocumentHandler_ReplaceContent_EmptyBody422(t *testing.T) {
	h, repo, storage, _ := newDocHandlerWithProcessor()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old", MimeType: testPptxMime}

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)
	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", strings.NewReader(""))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body: %s", rr.Code, rr.Body.String())
	}
	var resp RejectedContentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "EMPTY_CONTENT" {
		t.Errorf("code = %q, want EMPTY_CONTENT", resp.Code)
	}
	if storage.saved != nil {
		t.Error("blob written for rejected empty body")
	}
}

// US3/T017: concrete mismatch → 422 MIME_MISMATCH with the MIME pair.
func TestDocumentHandler_ReplaceContent_Mismatch422WithDetail(t *testing.T) {
	h, repo, storage, processor := newDocHandlerWithProcessor()
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old", MimeType: testPptxMime}
	processor.detectMIME = testDocxMime

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)
	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", strings.NewReader("a docx body"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body: %s", rr.Code, rr.Body.String())
	}
	var resp RejectedContentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "MIME_MISMATCH" {
		t.Errorf("code = %q, want MIME_MISMATCH", resp.Code)
	}
	if resp.Detail == nil || resp.Detail.KnownMime != testPptxMime || resp.Detail.DetectedMime != testDocxMime {
		t.Errorf("detail = %+v, want known=%q detected=%q", resp.Detail, testPptxMime, testDocxMime)
	}
	if storage.saved != nil {
		t.Error("blob written for rejected mismatched content")
	}
}

// TranscodeStream (stub for handler tests): pass-through copy; MIME echoes
// detectMIME override or the input type.
func (p *stubProcessor) TranscodeStream(r io.Reader, w io.Writer, mimeType string) (port.TranscodeResult, error) {
	p.transcodeCalls++
	if _, err := io.Copy(w, r); err != nil {
		return port.TranscodeResult{}, err
	}
	measured := p.processMeasured || (p.processDimsW != nil && p.processDimsH != nil)
	return port.TranscodeResult{MimeType: mimeType, ImageWidth: p.processDimsW, ImageHeight: p.processDimsH, Measured: measured}, nil
}

// US3/T022: replace path enforces the streaming cap (413 + counter path).
func TestDocumentHandler_ReplaceContent_OverLimit413(t *testing.T) {
	h, repo, storage := newDocHandler()
	h.MaxUploadSize = 1024
	docID := uuid.New()
	repo.doc = model.Document{ID: docID, ExternalID: "old", MimeType: "application/pdf"}

	r := chi.NewRouter()
	r.Put("/internal/file/{id}/content", h.ReplaceContent)
	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+docID.String()+"/content", bytes.NewReader(bytes.Repeat([]byte("z"), 8192)))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body: %s", rr.Code, rr.Body.String())
	}
	for _, st := range storage.stages {
		if !st.aborted {
			t.Error("stage not aborted after over-limit replace")
		}
	}
}

// Round-2 CR catch: oversized metadata fields must be rejected, not
// silently truncated into a different request.
func TestDocumentHandler_Create_OversizedFieldRejected(t *testing.T) {
	h, _, storage := newDocHandler()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "f.bin")
	_, _ = part.Write([]byte("content"))
	_ = writer.WriteField("displayName", strings.Repeat("x", (16<<10)+5))
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
	if !strings.Contains(rr.Body.String(), "16 KiB") {
		t.Errorf("body should name the field limit: %s", rr.Body.String())
	}
	// The file part precedes the oversized field, so a stage was already
	// open — the real invariant (FR-006) is that the early return aborted it.
	if len(storage.stages) == 0 {
		t.Fatal("expected a stage to be opened before the oversized field was rejected")
	}
	for _, st := range storage.stages {
		if !st.aborted {
			t.Error("stage not aborted after oversized-field rejection")
		}
	}
}

// M3 (013): the verbatim-store contract is byte-exact only when
// skipImageProcessing precedes the file part (the raw-store decision is taken
// at stage time). A skipImageProcessing=true that arrives AFTER the file part
// — when a transcodable image may already have been transcoded/rotated — must
// be rejected with 400 rather than silently transcoded, signalling the broken
// contract to the caller.
func TestDocumentHandler_Create_SkipImageProcessing_AfterFile_Rejected(t *testing.T) {
	h, _, storage, proc := newDocHandlerWithProcessor()
	proc.detectMIME = "image/png" // a transcodable type → the silent-transcode risk

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "photo.png")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\npayload"))
	// skipImageProcessing arrives AFTER the file — the misordered case.
	_ = writer.WriteField("skipImageProcessing", "true")
	_ = writer.WriteField("displayName", "photo.png")
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
		t.Fatalf("status = %d, want 400 for misordered skipImageProcessing, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "skipImageProcessing must be sent before the file part") {
		t.Errorf("body = %q, want the ordering-contract message", rr.Body.String())
	}
	// FR-006: the early return must abort the already-open stage.
	for _, st := range storage.stages {
		if !st.aborted {
			t.Error("stage not aborted after misordered-skip rejection")
		}
	}
}

// Counterpart to the misordered case: skipImageProcessing=true sent BEFORE the
// file part is honored — the verbatim arm runs (no transcode) and the upload
// succeeds. Proves the guard rejects only the unsafe ordering.
func TestDocumentHandler_Create_SkipImageProcessing_BeforeFile_Verbatim(t *testing.T) {
	h, _, _, proc := newDocHandlerWithProcessor()
	proc.detectMIME = "image/png"

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("skipImageProcessing", "true") // metadata first
	_ = writer.WriteField("displayName", "photo.png")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	part, _ := writer.CreateFormFile("file", "photo.png")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\npayload"))
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
	if proc.transcodeCalls != 0 {
		t.Errorf("transcodeCalls = %d, want 0 (verbatim must not transcode)", proc.transcodeCalls)
	}
}

// A skipImageProcessing=false arriving after the file part is harmless (false
// is the staged default), so it must NOT be rejected — only true is unsafe.
func TestDocumentHandler_Create_SkipImageProcessingFalse_AfterFile_Allowed(t *testing.T) {
	h, _, _, proc := newDocHandlerWithProcessor()
	proc.detectMIME = "image/png"

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "photo.png")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\npayload"))
	_ = writer.WriteField("skipImageProcessing", "false")
	_ = writer.WriteField("displayName", "photo.png")
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
		t.Fatalf("status = %d, want 201 for skip=false after file, body: %s", rr.Code, rr.Body.String())
	}
}

// --- 013: by-reference lookup + PATCH move (re-attribute) ---

// withID attaches a chi route context carrying the "id" URL param, so a
// handler that reads chi.URLParam("id") can be invoked directly without a router.
func withID(req *http.Request, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// T004: GET /internal/file/by-reference resolves GLOBALLY (bucketId omitted)
// and bucket-SCOPED (bucketId present), and 404s on a miss.
func TestByReference_GlobalAndScoped(t *testing.T) {
	ref := "synapse-media-id-42"
	bucket := uuid.New()
	docID := uuid.New()
	refDoc := model.Document{
		ID:                docID,
		ExternalID:        "hash-abc",
		MimeType:          "image/jpeg",
		Size:              123,
		StorageBucketID:   bucket,
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
	}

	t.Run("global (no bucketId)", func(t *testing.T) {
		h, repo, _ := newDocHandler()
		repo.refDoc = &refDoc
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref="+ref, nil)
		w := httptest.NewRecorder()
		h.ByReference(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if repo.lastRefBucket != nil {
			t.Errorf("global lookup must not pass a bucket; got %v", *repo.lastRefBucket)
		}
		if repo.lastRefValue != ref {
			t.Errorf("ref = %q, want %q", repo.lastRefValue, ref)
		}
		var got DocumentMetaResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != docID.String() {
			t.Errorf("id = %q, want %q", got.ID, docID.String())
		}
		if got.ExternalReference == nil || *got.ExternalReference != ref {
			t.Errorf("externalReference = %v, want %q", got.ExternalReference, ref)
		}
	})

	t.Run("scoped (bucketId present)", func(t *testing.T) {
		h, repo, _ := newDocHandler()
		repo.refDoc = &refDoc
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref="+ref+"&bucketId="+bucket.String(), nil)
		w := httptest.NewRecorder()
		h.ByReference(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if repo.lastRefBucket == nil || *repo.lastRefBucket != bucket {
			t.Errorf("scoped lookup must pass bucket %v; got %v", bucket, repo.lastRefBucket)
		}
	})

	t.Run("missing ref → 400", func(t *testing.T) {
		h, _, _ := newDocHandler()
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference", nil)
		w := httptest.NewRecorder()
		h.ByReference(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("invalid bucketId → 400", func(t *testing.T) {
		h, _, _ := newDocHandler()
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=x&bucketId=not-a-uuid", nil)
		w := httptest.NewRecorder()
		h.ByReference(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("not found → 404", func(t *testing.T) {
		h, repo, _ := newDocHandler()
		repo.refDoc = nil // → ErrDocumentNotFound
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=missing", nil)
		w := httptest.NewRecorder()
		h.ByReference(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}

// T008: PATCH /internal/file/{id} re-homes: sets storageBucketId, authorizationId,
// createdBy, and externalReference in one call (the inbound move primitive).
func TestUpdate_MovePlusReattribute(t *testing.T) {
	docID := uuid.New()
	newBucket := uuid.New()
	newAuth := uuid.New()
	newCreatedBy := uuid.New()
	newRef := "media-id-moved"

	h, repo, _ := newDocHandler()
	repo.doc = model.Document{
		ID:              docID,
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		Version:         1,
		MimeType:        "image/jpeg",
	}

	body := fmt.Sprintf(`{"storageBucketId":%q,"authorizationId":%q,"createdBy":%q,"externalReference":%q}`,
		newBucket, newAuth, newCreatedBy, newRef)
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(body))
	req = withID(req, docID.String())
	w := httptest.NewRecorder()
	h.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if repo.lastUpdateBucketID != newBucket {
		t.Errorf("bucket = %v, want %v", repo.lastUpdateBucketID, newBucket)
	}
	if repo.lastUpdateAuthID == nil || *repo.lastUpdateAuthID != newAuth {
		t.Errorf("authorizationId = %v, want %v", repo.lastUpdateAuthID, newAuth)
	}
	if repo.lastUpdateCreatedBy == nil || *repo.lastUpdateCreatedBy != newCreatedBy {
		t.Errorf("createdBy = %v, want %v", repo.lastUpdateCreatedBy, newCreatedBy)
	}
	if repo.lastUpdateExternalRef == nil || *repo.lastUpdateExternalRef != newRef {
		t.Errorf("externalReference = %v, want %q", repo.lastUpdateExternalRef, newRef)
	}
}

// externalReference is tri-state on PATCH: explicit null clears it.
func TestUpdate_ClearExternalReference(t *testing.T) {
	docID := uuid.New()
	existingRef := "old-ref"

	h, repo, _ := newDocHandler()
	repo.doc = model.Document{
		ID:                docID,
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		Version:           1,
		ExternalReference: &existingRef,
		MimeType:          "image/jpeg",
	}

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(`{"externalReference":null}`))
	req = withID(req, docID.String())
	w := httptest.NewRecorder()
	h.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if repo.lastUpdateExternalRef != nil {
		t.Errorf("externalReference = %v, want nil (explicit null clears)", *repo.lastUpdateExternalRef)
	}
}

// A PATCH that doesn't touch authorizationId must leave a NULL-auth row NULL,
// not rewrite it to the zero UUID (which would collide on UNIQUE(authorizationId)
// and 409 a second such row). Symmetric with the nullable createdBy seed.
func TestUpdate_LeavesNullAuthorizationIDNull(t *testing.T) {
	docID := uuid.New()

	h, repo, _ := newDocHandler()
	repo.doc = model.Document{
		ID:              docID,
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.Nil, // NULL authorizationId column
		Version:         1,
		MimeType:        "image/jpeg",
	}

	// PATCH only displayName — authorizationId omitted from the body.
	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(`{"displayName":"renamed.jpg"}`))
	req = withID(req, docID.String())
	w := httptest.NewRecorder()
	h.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if repo.lastUpdateAuthID != nil {
		t.Errorf("authorizationId = %v, want nil (NULL must stay NULL, not become the zero UUID)", *repo.lastUpdateAuthID)
	}
}

// Omitting externalReference keeps the row's current value (not cleared).
func TestUpdate_KeepsExternalReferenceWhenOmitted(t *testing.T) {
	docID := uuid.New()
	existingRef := "keep-me"

	h, repo, _ := newDocHandler()
	repo.doc = model.Document{
		ID:                docID,
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		Version:           1,
		ExternalReference: &existingRef,
		MimeType:          "image/jpeg",
	}

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+docID.String(), strings.NewReader(`{"displayName":"renamed.jpg"}`))
	req = withID(req, docID.String())
	w := httptest.NewRecorder()
	h.Update(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if repo.lastUpdateExternalRef == nil || *repo.lastUpdateExternalRef != existingRef {
		t.Errorf("externalReference = %v, want %q (omitted ⇒ keep)", repo.lastUpdateExternalRef, existingRef)
	}
}

// --- 013 review round 1 regression tests ---

// FIX 1 (PATCH): an empty-string externalReference is neither "keep" (absent)
// nor "clear" (explicit null), so it is rejected with 400 rather than persisted
// as a non-NULL ” that would orphan the row from both identity systems.
func TestUpdate_EmptyExternalReference_Rejected(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"externalReference":""}`, func(r *mockDocRepo) {
		r.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty externalReference; body=%s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("updateMetadataCalls = %d, want 0 (rejected before write)", repo.updateMetadataCalls)
	}
}

// FIX 1 (Copy): an empty externalReference is normalized to NULL — exactly like
// Create — so a later identical-content upload can content-dedup against it.
func TestCopy_EmptyExternalReference_PersistedAsNull(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:          sourceID,
		ExternalID:  "sha3-of-content",
		MimeType:    "image/png",
		Size:        42,
		DisplayName: "banner.png",
	}

	empty := ""
	body, _ := json.Marshal(CopyDocumentRequest{
		SourceID:            sourceID.String(),
		DestinationBucketID: uuid.New().String(),
		AuthorizationID:     uuid.New().String(),
		ExternalReference:   &empty,
	})

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ExternalReference != nil {
		t.Errorf("persisted externalReference = %v, want nil (empty ⇒ NULL)", *repo.lastCreateDoc.ExternalReference)
	}
}

// FIX 2: a reference-bearing create that collides on a DIFFERENT unique
// constraint (e.g. authorizationId) re-queries by reference, finds no winner,
// and must surface a clean 409 — not a 500 "winner lookup failed".
func TestCreate_ReferenceCreate_NonReferenceCollision_Returns409(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.createErr = model.ErrDuplicateKey // dup-key on a non-reference constraint
	// refDoc left nil ⇒ GetByReferenceInBucket returns ErrDocumentNotFound (no winner).

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "test.txt")
	_, _ = part.Write([]byte("hello"))
	_ = writer.WriteField("displayName", "test.txt")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	_ = writer.WriteField("externalReference", "fresh-media-id") // reference is new
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (collision on a different constraint), body: %s", rr.Code, rr.Body.String())
	}
}

// FIX 3: a body whose only key is an explicit-null on a non-clearable field
// merges nothing, so it is an idempotent 200 no-write — NOT a 400 — with the
// current document echoed back and no version+updatedDate bump.
func TestUpdate_ExplicitNullNoOp_Idempotent200(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"temporaryLocation":null}`, func(r *mockDocRepo) {
		r.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for no-op explicit null; body=%s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("updateMetadataCalls = %d, want 0 (no write on no-op)", repo.updateMetadataCalls)
	}
	var resp UpdateDocumentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != docID.String() {
		t.Errorf("id = %q, want %q (current document echoed back)", resp.ID, docID.String())
	}
}

// FIX 4: externalReference is capped at the VARCHAR(256) column on every path,
// so an over-length value is a clean 400 rather than a Postgres "value too
// long" 500.
func TestExternalReference_TooLong_Rejected(t *testing.T) {
	tooLong := strings.Repeat("a", maxExternalReferenceLen+1)

	t.Run("create", func(t *testing.T) {
		h, _, _ := newDocHandler()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, _ := writer.CreateFormFile("file", "test.txt")
		_, _ = part.Write([]byte("hello"))
		_ = writer.WriteField("displayName", "test.txt")
		_ = writer.WriteField("storageBucketId", uuid.New().String())
		_ = writer.WriteField("authorizationId", uuid.New().String())
		_ = writer.WriteField("externalReference", tooLong)
		_ = writer.Close()

		r := chi.NewRouter()
		r.Post("/internal/file", h.Create)
		req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("copy", func(t *testing.T) {
		h, repo, _ := newDocHandler()
		sourceID := uuid.New()
		repo.doc = model.Document{ID: sourceID, ExternalID: "sha3", MimeType: "image/png", Size: 1}
		body, _ := json.Marshal(CopyDocumentRequest{
			SourceID:            sourceID.String(),
			DestinationBucketID: uuid.New().String(),
			AuthorizationID:     uuid.New().String(),
			ExternalReference:   &tooLong,
		})
		r := chi.NewRouter()
		r.Post("/internal/file/copy", h.Copy)
		req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("patch", func(t *testing.T) {
		docID := uuid.New()
		rr, repo := runPatch(t, docID, `{"externalReference":"`+tooLong+`"}`, func(r *mockDocRepo) {
			r.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		if repo.updateMetadataCalls != 0 {
			t.Errorf("updateMetadataCalls = %d, want 0 (rejected before write)", repo.updateMetadataCalls)
		}
	})
}

// FIX 5: a duplicated skipImageProcessing=true — sent BEFORE the file and again
// AFTER — honors the byte-exact contract (the pre-file value already established
// verbatim), so the after-file duplicate must NOT trip the ordering guard.
func TestDocumentHandler_Create_SkipImageProcessing_DuplicatedConsistent_Allowed(t *testing.T) {
	h, _, _, proc := newDocHandlerWithProcessor()
	proc.detectMIME = "image/png"

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("skipImageProcessing", "true") // established before the file
	_ = writer.WriteField("displayName", "photo.png")
	_ = writer.WriteField("storageBucketId", uuid.New().String())
	_ = writer.WriteField("authorizationId", uuid.New().String())
	part, _ := writer.CreateFormFile("file", "photo.png")
	_, _ = part.Write([]byte("\x89PNG\r\n\x1a\npayload"))
	_ = writer.WriteField("skipImageProcessing", "true") // duplicate, consistent with pre-file value
	_ = writer.Close()

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for consistent duplicate skip, body: %s", rr.Code, rr.Body.String())
	}
	if proc.transcodeCalls != 0 {
		t.Errorf("transcodeCalls = %d, want 0 (verbatim honored)", proc.transcodeCalls)
	}
}

// FIX 7: an over-cap PATCH body is bounded by http.MaxBytesReader and maps to
// 413 rather than buffering unbounded input.
func TestUpdate_OversizedBody_Rejected(t *testing.T) {
	docID := uuid.New()
	huge := strings.Repeat("a", (1<<20)+1024) // just over the 1 MiB cap
	rr, repo := runPatch(t, docID, `{"displayName":"`+huge+`"}`, func(r *mockDocRepo) {
		r.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 for oversized PATCH body; body=%s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("updateMetadataCalls = %d, want 0 (rejected before write)", repo.updateMetadataCalls)
	}
}

func TestBuildMetadataUpdate_AuthorizationIdCannotBeCleared(t *testing.T) {
	base := model.Document{
		ID:              uuid.New(),
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
	}
	present := map[string]struct{}{"authorizationId": {}}

	// Explicit JSON null → clear → rejected (would orphan the document's auth).
	if _, _, err := buildMetadataUpdate(base, UpdateDocumentRequest{AuthorizationID: nil}, present); err == nil || !strings.Contains(err.Error(), "cannot be cleared") {
		t.Fatalf("null authorizationId: want cannot-be-cleared error, got %v", err)
	}

	// Nil UUID string → rejected (reads back as the zero UUID → 403 forever).
	zero := uuid.Nil.String()
	if _, _, err := buildMetadataUpdate(base, UpdateDocumentRequest{AuthorizationID: &zero}, present); err == nil || !strings.Contains(err.Error(), "nil UUID") {
		t.Fatalf("nil-UUID authorizationId: want nil-UUID error, got %v", err)
	}

	// Valid re-attribution → applied.
	newAuth := uuid.New().String()
	meta, applied, err := buildMetadataUpdate(base, UpdateDocumentRequest{AuthorizationID: &newAuth}, present)
	if err != nil || applied != 1 || meta.AuthorizationID == nil || meta.AuthorizationID.String() != newAuth {
		t.Fatalf("valid re-attribution: applied=%d err=%v authz=%v", applied, err, meta.AuthorizationID)
	}
}
