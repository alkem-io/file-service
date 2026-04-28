package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

type mockDocRepo struct {
	doc          model.Document
	err          error
	findDoc      *model.Document // non-nil → FindByExternalIDAndBucket returns this (dedup hit)
	findErr      error
	createErr    error
	updateErr    error
	deleteResult model.DeletedDocument
	deleteErr    error
	count        int
}

func (m *mockDocRepo) GetByID(_ context.Context, _ uuid.UUID) (model.Document, error) {
	return m.doc, m.err
}
func (m *mockDocRepo) FindByExternalIDAndBucket(_ context.Context, _ string, _ uuid.UUID) (model.Document, error) {
	if m.findErr != nil {
		return model.Document{}, m.findErr
	}
	if m.findDoc != nil {
		return *m.findDoc, nil
	}
	// Default to "not found" so CreateDocument proceeds to insert.
	return model.Document{}, model.ErrDocumentNotFound
}
func (m *mockDocRepo) Create(_ context.Context, doc model.Document) (uuid.UUID, error) {
	return doc.ID, m.createErr
}
func (m *mockDocRepo) UpdateFile(_ context.Context, _ uuid.UUID, _, _ string, _ int) error {
	return m.updateErr
}
func (m *mockDocRepo) UpdateLocation(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ bool, _ int) error {
	return m.updateErr
}
func (m *mockDocRepo) Delete(_ context.Context, _ uuid.UUID) (model.DeletedDocument, error) {
	return m.deleteResult, m.deleteErr
}
func (m *mockDocRepo) CountByExternalID(_ context.Context, _ string) (int, error) {
	return m.count, nil
}

type mockAuth struct {
	result model.AuthResult
	err    error

	// Captured args from the most recent CheckPrivilege call.
	calls            int
	lastActorID      string
	lastPrivilege    string
	lastAuthPolicyID string
}

func (m *mockAuth) CheckPrivilege(_ context.Context, actorID, privilege, authPolicyID string) (model.AuthResult, error) {
	m.calls++
	m.lastActorID = actorID
	m.lastPrivilege = privilege
	m.lastAuthPolicyID = authPolicyID
	return m.result, m.err
}

type mockStorage struct {
	data    []byte
	err     error
	saveErr error
}

func (m *mockStorage) Save(content []byte) (model.StoredFile, error) {
	if m.saveErr != nil {
		return model.StoredFile{}, m.saveErr
	}
	return model.StoredFile{ExternalID: "hash", Size: len(content), Created: true}, nil
}
func (m *mockStorage) Read(_ string) ([]byte, error) { return m.data, m.err }
func (m *mockStorage) Delete(_ string) error         { return nil }
func (m *mockStorage) Exists(_ string) (bool, error) { return m.data != nil, nil }

func TestPublicHandler_Authorized(t *testing.T) {
	docID := uuid.New()
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              docID,
			ExternalID:      "abc123",
			MimeType:        "application/pdf",
			AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{data: []byte("file-content")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want %q", cc, "public, max-age=86400")
	}
	if pragma := rr.Header().Get("Pragma"); pragma != "public" {
		t.Errorf("Pragma = %q, want %q", pragma, "public")
	}
	if expires := rr.Header().Get("Expires"); expires == "" {
		t.Error("missing Expires header")
	}
	wantETag := `"abc123"` // ETag is based on externalID (content hash)
	if etag := rr.Header().Get("ETag"); etag != wantETag {
		t.Errorf("ETag = %q, want %q", etag, wantETag)
	}
	if rr.Body.String() != "file-content" {
		t.Errorf("body = %q, want %q", rr.Body.String(), "file-content")
	}
}

func TestPublicHandler_Unauthorized(t *testing.T) {
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              uuid.New(),
			AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: false, Reason: "denied"}},
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

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestPublicHandler_DocumentNotFound(t *testing.T) {
	h := &PublicHandler{
		Repo:    &mockDocRepo{err: model.ErrDocumentNotFound},
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

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestPublicHandler_FileNotFound(t *testing.T) {
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              uuid.New(),
			ExternalID:      "missing",
			AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{err: os.ErrNotExist},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestPublicHandler_ConditionalRequest304(t *testing.T) {
	docID := uuid.New()
	h := &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID:              docID,
			ExternalID:      "abc123",
			MimeType:        "application/pdf",
			AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{data: []byte("file-content")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	req.Header.Set("If-None-Match", `"abc123"`) // match externalID
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rr.Code)
	}
}
