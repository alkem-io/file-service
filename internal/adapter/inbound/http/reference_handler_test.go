package http

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// ptr is a small generic helper for the optional-value document fields.
func ptr[T any](v T) *T { return &v }

// --- by-reference lookup (GET /internal/file/by-reference) ---

func TestByReference_GlobalResolvesAcrossBuckets(t *testing.T) {
	h, repo, _ := newDocHandler()
	w, ht := 320, 240
	repo.refDoc = &model.Document{
		ID:                uuid.New(),
		ExternalID:        "hashabc",
		MimeType:          "image/png",
		Size:              99,
		DisplayName:       "m.png",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: ptr("media_id_1"),
		ImageWidth:        &w,
		ImageHeight:       &ht,
	}

	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=media_id_1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.refCalls != 1 || repo.lastRefKey != "media_id_1" {
		t.Errorf("GetByReference not called with the ref: calls=%d key=%q", repo.refCalls, repo.lastRefKey)
	}
	body := rr.Body.String()
	for _, want := range []string{`"externalReference":"media_id_1"`, `"imageWidth":320`, `"imageHeight":240`, `"externalID":"hashabc"`} {
		if !strings.Contains(body, want) {
			t.Errorf("by-reference response missing %s; body=%s", want, body)
		}
	}
}

func TestByReference_ScopedUsesBucketVariant(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.refDoc = &model.Document{ID: uuid.New(), ExternalID: "h", MimeType: "text/plain", ExternalReference: ptr("media_id_2")}

	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=media_id_2&bucketId="+uuid.New().String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if repo.refCalls != 1 {
		t.Errorf("scoped by-reference did not resolve via the repo (calls=%d)", repo.refCalls)
	}
}

func TestByReference_MissingRefIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing ref", rr.Code)
	}
}

func TestByReference_InvalidBucketIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=x&bucketId=not-a-uuid", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid bucketId", rr.Code)
	}
}

func TestByReference_NotFoundIs404(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.refDoc = nil // → ErrDocumentNotFound
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// --- create: externalReference, optional authorizationId, skip ordering ---

// buildCreateBody writes an ordered multipart body: the metadata fields in the
// given order, followed by the file part LAST when fileLast=true, else the file
// FIRST. Ordering matters only for skipImageProcessing (consumed when the file
// is staged).
func buildCreateBody(t *testing.T, fields [][2]string, fileLast bool, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	wr := multipart.NewWriter(&body)
	writeFile := func() {
		part, _ := wr.CreateFormFile("file", "blob")
		_, _ = part.Write(content)
	}
	if !fileLast {
		writeFile()
	}
	for _, f := range fields {
		_ = wr.WriteField(f[0], f[1])
	}
	if fileLast {
		writeFile()
	}
	_ = wr.Close()
	return &body, wr.FormDataContentType()
}

func TestCreate_PersistsExternalReference(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		{"externalReference", "media_id_abc"},
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ExternalReference == nil || *repo.lastCreateDoc.ExternalReference != "media_id_abc" {
		t.Errorf("externalReference not persisted: %v", repo.lastCreateDoc.ExternalReference)
	}
}

// The Synapse media provider stores staging docs with NO authorizationId; the
// server mints one on inbound re-home. Create must accept an omitted
// authorizationId (201) and store the zero UUID → NULL.
func TestCreate_OmittedAuthorizationId_StoresNullAuth(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"externalReference", "media_id_blob"},
		{"skipImageProcessing", "true"},
		// authorizationId intentionally omitted
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.AuthorizationID != uuid.Nil {
		t.Errorf("AuthorizationID = %v, want uuid.Nil (NULL) when omitted", repo.lastCreateDoc.AuthorizationID)
	}
}

// skipImageProcessing=true established AFTER the file part is rejected — the
// bytes may already have been transcoded, so the byte-exact contract cannot be
// honored.
func TestCreate_SkipImageProcessingAfterFileIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	// file FIRST, skipImageProcessing after.
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		{"skipImageProcessing", "true"},
	}, false, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (skipImageProcessing after file), body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "skipImageProcessing must be sent before the file part") {
		t.Errorf("body = %q, want the ordering error", rr.Body.String())
	}
}

// --- PATCH move + re-attribute ---

func TestPatch_ReattributeAuthorizationId(t *testing.T) {
	docID := uuid.New()
	newAuth := uuid.New()
	rr, repo := runPatch(t, docID, `{"authorizationId":"`+newAuth.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 1 {
		t.Fatalf("UpdateMetadata calls = %d, want 1", repo.updateMetadataCalls)
	}
	if repo.lastUpdateMeta.AuthorizationID == nil || *repo.lastUpdateMeta.AuthorizationID != newAuth {
		t.Errorf("re-attributed authorizationId = %v, want %v", repo.lastUpdateMeta.AuthorizationID, newAuth)
	}
}

func TestPatch_AuthorizationIdCannotBeCleared(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"authorizationId":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (auth cannot be cleared), body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("rejected PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

func TestPatch_ExternalReference_SetAndClear(t *testing.T) {
	docID := uuid.New()

	// set
	rr, repo := runPatch(t, docID, `{"externalReference":"media_id_new"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("set: status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateMeta.ExternalReference == nil || *repo.lastUpdateMeta.ExternalReference != "media_id_new" {
		t.Errorf("externalReference set = %v, want media_id_new", repo.lastUpdateMeta.ExternalReference)
	}

	// clear via explicit null
	rr2, repo2 := runPatch(t, docID, `{"externalReference":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), ExternalReference: ptr("media_id_old"), Version: 1}
	})
	if rr2.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, body: %s", rr2.Code, rr2.Body.String())
	}
	if repo2.updateMetadataCalls != 1 || repo2.lastUpdateMeta.ExternalReference != nil {
		t.Errorf("externalReference not cleared: calls=%d ref=%v", repo2.updateMetadataCalls, repo2.lastUpdateMeta.ExternalReference)
	}
}

func TestPatch_ExternalReference_EmptyStringRejected(t *testing.T) {
	docID := uuid.New()
	rr, _ := runPatch(t, docID, `{"externalReference":""}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty externalReference, body: %s", rr.Code, rr.Body.String())
	}
}

// A case-variant JSON key must still resolve to the canonical field and apply
// its re-attribute — otherwise a half-applied, security-relevant re-home could
// silently drop the change.
func TestPatch_CaseInsensitiveKey_AppliesReattribute(t *testing.T) {
	docID := uuid.New()
	newAuth := uuid.New()
	rr, repo := runPatch(t, docID, `{"AuthorizationId":"`+newAuth.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateMeta.AuthorizationID == nil || *repo.lastUpdateMeta.AuthorizationID != newAuth {
		t.Errorf("case-variant key dropped the re-attribute: %v", repo.lastUpdateMeta.AuthorizationID)
	}
}

// An explicit-null on an already-null field is a no-op → idempotent 200, no write.
func TestPatch_ExplicitNullNoOp_Is200NoWrite(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"createdBy":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1} // CreatedBy already nil
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 idempotent no-op, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("no-op PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

// --- meta response carries externalReference + dims ---

func TestGetMeta_IncludesExternalReferenceAndDims(t *testing.T) {
	h, repo, _ := newDocHandler()
	w, ht := 128, 96
	docID := uuid.New()
	repo.doc = model.Document{
		ID: docID, ExternalID: "h", MimeType: "image/png", StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(), ExternalReference: ptr("media_id_meta"), ImageWidth: &w, ImageHeight: &ht,
	}
	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+docID.String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`"externalReference":"media_id_meta"`, `"imageWidth":128`, `"imageHeight":96`} {
		if !strings.Contains(body, want) {
			t.Errorf("meta response missing %s; body=%s", want, body)
		}
	}
}
