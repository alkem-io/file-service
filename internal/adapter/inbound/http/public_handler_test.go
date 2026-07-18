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

	// Captured args from the most recent UpdateMetadata call.
	updateMetadataCalls   int
	lastUpdateBucketID    uuid.UUID
	lastUpdateTemporary   bool
	lastUpdateDisplayName string
	lastUpdateVersion     int
	lastUpdateAuthID      *uuid.UUID
	lastUpdateCreatedBy   *uuid.UUID
	lastUpdateExternalRef *string

	// by-reference lookups (013): refDoc served by both Get*ByReference;
	// nil → ErrDocumentNotFound. refErr overrides. lastRefBucket records the
	// bucket passed to the scoped form (nil ⇒ global form was used).
	refDoc        *model.Document
	refErr        error
	lastRefValue  string
	lastRefBucket *uuid.UUID

	// Captured args from Create / UpdateFile content_metadata params (US1+).
	lastCreateContentMetadata     model.ContentMetadata
	lastUpdateFileContentMetadata model.ContentMetadata

	// Captured full doc from the most recent Create (013: asserts the
	// persisted externalReference is normalized to NULL on empty input).
	lastCreateDoc model.Document

	// Lazy-backfill capture (US1).
	backfillCalls          int
	lastBackfillID         uuid.UUID
	lastBackfillExternalID string
	lastBackfillPayload    model.ContentMetadata
	backfillErr            error
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
func (m *mockDocRepo) Create(_ context.Context, doc model.Document, contentMetadata model.ContentMetadata) (uuid.UUID, error) {
	m.lastCreateContentMetadata = contentMetadata
	m.lastCreateDoc = doc
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
func (m *mockDocRepo) UpdateMetadata(_ context.Context, _ uuid.UUID, meta model.DocumentMetadataUpdate, version int) (model.Document, error) {
	m.updateMetadataCalls++
	m.lastUpdateBucketID = meta.StorageBucketID
	m.lastUpdateTemporary = meta.TemporaryLocation
	m.lastUpdateDisplayName = meta.DisplayName
	m.lastUpdateVersion = version
	m.lastUpdateAuthID = meta.AuthorizationID
	m.lastUpdateCreatedBy = meta.CreatedBy
	m.lastUpdateExternalRef = meta.ExternalReference
	if m.updateErr != nil {
		return model.Document{}, m.updateErr
	}
	// Mirror the adapter's full-row RETURNING: the post-update row reflects the
	// applied metadata SETs; content fields (externalID/size/mime/dims/
	// content_metadata) are untouched by a metadata PATCH, so they carry over
	// from the current row. The service builds the whole PATCH response from this
	// one authoritative snapshot — no post-update re-read.
	updated := m.doc
	updated.StorageBucketID = meta.StorageBucketID
	updated.TemporaryLocation = meta.TemporaryLocation
	updated.DisplayName = meta.DisplayName
	updated.CreatedBy = meta.CreatedBy
	updated.ExternalReference = meta.ExternalReference
	if meta.AuthorizationID != nil {
		updated.AuthorizationID = *meta.AuthorizationID
	} else {
		updated.AuthorizationID = uuid.Nil
	}
	updated.Version = version + 1
	return updated, nil
}
func (m *mockDocRepo) GetByReference(_ context.Context, reference string) (model.Document, error) {
	m.lastRefValue = reference
	m.lastRefBucket = nil
	if m.refErr != nil {
		return model.Document{}, m.refErr
	}
	if m.refDoc != nil {
		return *m.refDoc, nil
	}
	return model.Document{}, model.ErrDocumentNotFound
}
func (m *mockDocRepo) GetByReferenceInBucket(_ context.Context, reference string, bucketID uuid.UUID) (model.Document, error) {
	m.lastRefValue = reference
	b := bucketID
	m.lastRefBucket = &b
	if m.refErr != nil {
		return model.Document{}, m.refErr
	}
	if m.refDoc != nil {
		return *m.refDoc, nil
	}
	return model.Document{}, model.ErrDocumentNotFound
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
}

func (m *mockStorage) Save(content []byte) (model.StoredFile, error) {
	m.saved = content
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

// Stored-XSS hardening (deny-list + nosniff; see activeContentMIME): the public
// serve endpoint always sets X-Content-Type-Options: nosniff and force-downloads
// ONLY the enumerable set of browser-active/script-capable content-types
// (html/xhtml/svg, the XML family, syndication/RDF/MathML, MHTML/rfc822).
// Everything else — inert media, documents, unknown types — serves inline for
// in-browser preview parity with the TS file-service. This is the settled
// design; the inline cases below include the delta-2 regression media types.
func TestPublicHandler_ServeDisposition(t *testing.T) {
	cases := []struct {
		mime            string
		wantDisposition string
	}{
		// Inert media / documents → inline (safe under nosniff, TS-parity preview).
		{"image/png", "inline"},
		{"image/jpeg", "inline"},
		{"image/gif", "inline"},
		{"image/webp", "inline"},
		{"image/avif", "inline"},
		{"application/pdf", "inline"},
		{"text/plain", "inline"},
		// delta-2 regression cases: an allow-list wrongly force-downloaded these.
		{"image/bmp", "inline"},
		{"image/tiff", "inline"},
		{"image/heic", "inline"},
		{"video/mp4", "inline"},
		{"audio/mpeg", "inline"},
		{"application/json", "inline"},
		{"text/csv", "inline"},
		{"application/octet-stream", "inline"},   // inert binary; nosniff prevents active render
		{"application/x-unknown-type", "inline"}, // unknown/inert → inline is safe under nosniff
		{"image/png; qs=0.5", "inline"},          // params stripped before match
		{"IMAGE/PNG", "inline"},                  // normalization: case-insensitive match
		// Browser-active / script-capable content-types → attachment.
		{"text/html", "attachment"},
		{"application/xhtml+xml", "attachment"},
		{"image/svg+xml", "attachment"},
		{"application/xml", "attachment"},
		{"text/xml", "attachment"},
		{"application/rss+xml", "attachment"},
		{"application/atom+xml", "attachment"},
		{"application/xslt+xml", "attachment"}, // XSLT stylesheet can execute generated script
		{"text/xsl", "attachment"},             // legacy XSLT alias
		{"message/rfc822", "attachment"},
		{"text/html; charset=utf-8", "attachment"}, // params stripped before match
		{"IMAGE/SVG+XML", "attachment"},            // normalization: case-insensitive match
	}
	for _, tc := range cases {
		t.Run(tc.mime, func(t *testing.T) {
			docID := uuid.New()
			h := &PublicHandler{
				Repo: &mockDocRepo{doc: model.Document{
					ID:              docID,
					ExternalID:      "abc123",
					MimeType:        tc.mime,
					AuthorizationID: uuid.New(),
				}},
				Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
				Storage: &mockStorage{data: []byte("file-content")},
				MaxAge:  86400,
				Logger:  zap.NewNop(),
			}

			r := chi.NewRouter()
			r.Get("/rest/storage/file/{id}", h.ServeDocument)
			req := httptest.NewRequest(http.MethodGet, "/rest/storage/file/"+docID.String(), nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := rr.Header().Get("Content-Disposition"); got != tc.wantDisposition {
				t.Errorf("Content-Disposition = %q, want %q for %s", got, tc.wantDisposition, tc.mime)
			}
		})
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
