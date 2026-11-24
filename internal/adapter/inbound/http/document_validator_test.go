package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// assertValidatorHidesStorageID checks the served validator is a strong entity
// tag that neither is nor contains the private storage identity (ADR 0012).
func assertValidatorHidesStorageID(t *testing.T, etag, externalID string) {
	t.Helper()
	if len(etag) < 3 || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %q, want a strong quoted entity tag", etag)
	}
	if externalID != "" && strings.Contains(etag, externalID) {
		t.Fatalf("ETag %q carries the storage identity %q", etag, externalID)
	}
}

// serveConditional runs one authenticated GET of the id route with optional headers.
func serveConditional(h *PublicHandler, docID uuid.UUID, header map[string]string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)
	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// Every write that changes the served representation bumps the row version, so
// a validator issued before the write must stop revalidating after it, even
// when the stored bytes (and hence the storage identity) are unchanged — a
// rename or a MIME correction changes the disposition or type a cache would
// otherwise keep serving.
func TestServeDocument_ValidatorChangesWithEachRevision(t *testing.T) {
	h, docID := newServeHandlerFor(model.Document{
		ExternalID: "shared-content-hash", MimeType: "image/png", DisplayName: "a.png", AuthorizationID: uuid.New(), Version: 3,
	})
	repo := h.Repo.(*mockDocRepo)

	first := serveConditional(h, docID, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", first.Code)
	}
	stale := first.Header().Get("ETag")
	assertValidatorHidesStorageID(t, stale, repo.doc.ExternalID)

	if rr := serveConditional(h, docID, map[string]string{"If-None-Match": stale}); rr.Code != http.StatusNotModified {
		t.Fatalf("unchanged revision: status = %d, want 304", rr.Code)
	}

	repo.doc.Version++
	repo.doc.DisplayName = "b.png"

	rr := serveConditional(h, docID, map[string]string{"If-None-Match": stale})
	if rr.Code != http.StatusOK {
		t.Fatalf("stale validator after a new revision: status = %d, want 200", rr.Code)
	}
	current := rr.Header().Get("ETag")
	if current == stale {
		t.Fatalf("validator %q did not change with the revision", current)
	}
	assertValidatorHidesStorageID(t, current, repo.doc.ExternalID)
	if rr := serveConditional(h, docID, map[string]string{"If-None-Match": current}); rr.Code != http.StatusNotModified {
		t.Fatalf("current validator: status = %d, want 304", rr.Code)
	}
}

// Deduplication lets two logical files share one stored blob. Their validators
// must still differ, so one file's cached copy never revalidates as the other.
func TestServeDocument_ValidatorIsDistinctPerFile(t *testing.T) {
	a := model.Document{ID: uuid.New(), ExternalID: "deduplicated-hash", MimeType: "image/png", AuthorizationID: uuid.New(), Version: 1}
	b := a
	b.ID = uuid.New()
	b.AuthorizationID = uuid.New()
	h := &PublicHandler{
		Repo:    &mockDocRepo{docsByID: map[uuid.UUID]model.Document{a.ID: a, b.ID: b}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{data: []byte("payload")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}

	etagA := serveConditional(h, a.ID, nil).Header().Get("ETag")
	etagB := serveConditional(h, b.ID, nil).Header().Get("ETag")
	assertValidatorHidesStorageID(t, etagA, a.ExternalID)
	assertValidatorHidesStorageID(t, etagB, b.ExternalID)
	if etagA == etagB {
		t.Fatalf("two files share the validator %q", etagA)
	}
	if rr := serveConditional(h, b.ID, map[string]string{"If-None-Match": etagA}); rr.Code != http.StatusOK {
		t.Fatalf("another file's validator: status = %d, want 200", rr.Code)
	}
}

// A file deleted and recreated under the same reference is a new logical file:
// the old file's validator must not revalidate it, even with identical bytes.
func TestServeByReference_RecreatedFileDoesNotRevalidateOldValidator(t *testing.T) {
	repo, _, _, router, ref := referenceFixture(t)
	path := "/rest/storage/file/by-reference?bucketId=" + repo.doc.StorageBucketID.String() + "&ref=" + ref

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, referenceRequest(http.MethodGet, path))
	if rr.Code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", rr.Code)
	}
	old := rr.Header().Get("ETag")
	assertValidatorHidesStorageID(t, old, repo.doc.ExternalID)

	recreated := *repo.refInBucketDoc
	recreated.ID = uuid.New()
	repo.refInBucketDoc = &recreated

	req := referenceRequest(http.MethodGet, path)
	req.Header.Set("If-None-Match", old)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("old validator on a recreated file: status = %d, want 200", rr.Code)
	}
	if rr.Header().Get("ETag") == old {
		t.Fatal("recreated file reuses the old file's validator")
	}
}

// A player resuming with If-Range after the file was revised must get the whole
// current representation, never a slice spliced onto its stale copy.
func TestServeDocument_IfRangeAfterRevisionServesWholeBlob(t *testing.T) {
	f := newRangeFixture(t)
	f.handler.Repo.(*mockDocRepo).doc.Version++

	rr := f.serve(map[string]string{"Range": "bytes=0-1", "If-Range": f.etag})
	if rr.Code != http.StatusOK {
		t.Fatalf("If-Range with a superseded validator: status = %d, want 200", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), f.payload) {
		t.Error("If-Range with a superseded validator: body is not the whole blob")
	}
}
