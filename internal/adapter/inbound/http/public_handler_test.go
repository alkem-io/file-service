package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
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

	// docsByID, when non-nil, makes GetByID id-aware: it returns the mapped
	// document for a known id and model.ErrDocumentNotFound for an unknown
	// one. Used by the batched-read tests, which need several distinct rows
	// in one request. When nil, GetByID falls back to the single doc/err
	// pair above so every existing single-document test is unaffected.
	docsByID map[uuid.UUID]model.Document

	// Captured args from the most recent UpdateMetadata call.
	updateMetadataCalls   int
	lastUpdateBucketID    uuid.UUID
	lastUpdateTemporary   bool
	lastUpdateDisplayName string
	lastUpdateVersion     int

	// Captured args from Create / UpdateFile content_metadata params (US1+).
	lastCreateContentMetadata     model.ContentMetadata
	lastUpdateFileContentMetadata model.ContentMetadata

	// Lazy-backfill capture (US1).
	backfillCalls          int
	lastBackfillID         uuid.UUID
	lastBackfillExternalID string
	lastBackfillPayload    model.ContentMetadata
	backfillErr            error
}

func (m *mockDocRepo) GetByID(_ context.Context, id uuid.UUID) (model.Document, error) {
	if m.docsByID != nil {
		if doc, ok := m.docsByID[id]; ok {
			return doc, nil
		}
		return model.Document{}, model.ErrDocumentNotFound
	}
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
func (m *mockDocRepo) Create(_ context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error) {
	m.lastCreateContentMetadata = contentMetadata
	return doc.ID, m.createErr
}
func (m *mockDocRepo) UpdateFile(_ context.Context, _ uuid.UUID, _, _ string, _ int, contentMetadata model.ContentMetadata) error {
	m.lastUpdateFileContentMetadata = contentMetadata
	return m.updateErr
}
func (m *mockDocRepo) BackfillContentMetadata(_ context.Context, id uuid.UUID, expectedExternalID string, metadata model.ContentMetadata) error {
	m.backfillCalls++
	m.lastBackfillID = id
	m.lastBackfillExternalID = expectedExternalID
	m.lastBackfillPayload = metadata
	return m.backfillErr
}
func (m *mockDocRepo) UpdateMetadata(_ context.Context, _ uuid.UUID, bucketID uuid.UUID, temporary bool, displayName string, version int) error {
	m.updateMetadataCalls++
	m.lastUpdateBucketID = bucketID
	m.lastUpdateTemporary = temporary
	m.lastUpdateDisplayName = displayName
	m.lastUpdateVersion = version
	if m.updateErr != nil {
		return m.updateErr
	}
	// Mirror real adapter behavior so the subsequent service GetByID
	// reflects the update — otherwise tests can't tell whether the
	// handler propagated the new values or returned stale ones.
	m.doc.StorageBucketID = bucketID
	m.doc.TemporaryLocation = temporary
	m.doc.DisplayName = displayName
	m.doc.Version = version + 1
	return nil
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
	saved   []byte // captures the last Save/stage payload (replace-path rejection tests)
	stages  []*httpMockStage

	// blobsByExternalID, when non-nil, makes Read externalID-aware: it returns
	// the mapped bytes for a known externalID and os.ErrNotExist for an
	// unknown one. Used by the batched-read tests so distinct documents map to
	// distinct blobs (and a "missing blob" case is expressible). When nil,
	// Read falls back to the single data/err pair so existing tests are
	// unaffected.
	blobsByExternalID map[string][]byte
}

func (m *mockStorage) Save(content []byte) (model.StoredFile, error) {
	m.saved = content
	if m.saveErr != nil {
		return model.StoredFile{}, m.saveErr
	}
	return model.StoredFile{ExternalID: "hash", Size: len(content), Created: true}, nil
}
func (m *mockStorage) Read(externalID string) ([]byte, error) {
	if m.blobsByExternalID != nil {
		if b, ok := m.blobsByExternalID[externalID]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	return m.data, m.err
}
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

func (m *mockDocRepo) ListByMimeTypes(_ context.Context, _ []string) ([]model.Document, error) {
	return nil, nil
}
func (m *mockDocRepo) UpdateMimeType(_ context.Context, _ uuid.UUID, _, _ string) (bool, error) {
	return true, nil
}

// httpMockStage backs mockStorage.OpenStage for handler-level tests (spec 020).
type httpMockStage struct {
	parent    *mockStorage
	buf       bytes.Buffer
	committed bool
	aborted   bool
}

func (s *httpMockStage) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *httpMockStage) Commit() (model.StoredFile, error) {
	if s.parent.saveErr != nil {
		return model.StoredFile{}, s.parent.saveErr
	}
	content := s.buf.Bytes()
	s.parent.saved = content
	s.committed = true
	return model.StoredFile{ExternalID: service.ComputeHash(content), Size: len(content), Created: true}, nil
}
func (s *httpMockStage) Abort() error { s.aborted = true; return nil }

// StagedReaderAt mirrors the real StageWriter contract: the staging artifact is
// only readable before Commit/Abort (the local adapter's temp file is gone
// afterwards), so reading post-lifecycle is an error rather than a stale buffer.
func (s *httpMockStage) StagedReaderAt() (io.ReaderAt, int64, error) {
	if s.committed || s.aborted {
		return nil, 0, errors.New("httpMockStage: reader after commit/abort")
	}
	b := s.buf.Bytes()
	return bytes.NewReader(b), int64(len(b)), nil
}

func (m *mockStorage) OpenStage(_ context.Context) (port.StageWriter, error) {
	st := &httpMockStage{parent: m}
	m.stages = append(m.stages, st)
	return st, nil
}
