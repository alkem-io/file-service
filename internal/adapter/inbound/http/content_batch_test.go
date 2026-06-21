package http

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// runContentBatch wires a fresh handler + chi router and dispatches a single
// POST /internal/file/content-batch request with the given JSON body. The
// configure callback (if non-nil) seeds mock state on the repo + storage
// before the request is served. Returns the response recorder so callers can
// assert status + body without repeating chi/httptest boilerplate.
func runContentBatch(t *testing.T, body string, configure func(*mockDocRepo, *mockStorage)) *httptest.ResponseRecorder {
	t.Helper()
	h, repo, storage := newDocHandler()
	if configure != nil {
		configure(repo, storage)
	}
	r := chi.NewRouter()
	r.Post("/internal/file/content-batch", h.ContentBatch)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/content-batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func decodeBatchResponse(t *testing.T, rr *httptest.ResponseRecorder) ContentBatchResponse {
	t.Helper()
	var resp ContentBatchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	return resp
}

// Happy path: every requested id resolves to its own blob, decoded from the
// base64 payload. This is the core derived-text-resolver use case (N memo
// snapshot pointers → N snapshot blobs in one round trip).
func TestContentBatch_ReturnsEachBlob(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	want := map[uuid.UUID]string{id1: "snapshot-one", id2: "snapshot-two"}

	rr := runContentBatch(t, `{"ids":["`+id1.String()+`","`+id2.String()+`"]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{
			id1: {ID: id1, ExternalID: "ext-1", MimeType: "application/octet-stream"},
			id2: {ID: id2, ExternalID: "ext-2", MimeType: "text/plain"},
		}
		storage.blobsByExternalID = map[string][]byte{
			"ext-1": []byte(want[id1]),
			"ext-2": []byte(want[id2]),
		}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Items))
	}
	for _, item := range resp.Items {
		id := uuid.MustParse(item.ID)
		if !item.Found {
			t.Errorf("id %s: found = false, want true", id)
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(item.ContentBase64)
		if err != nil {
			t.Errorf("id %s: base64 decode: %v", id, err)
			continue
		}
		if string(decoded) != want[id] {
			t.Errorf("id %s: content = %q, want %q", id, decoded, want[id])
		}
	}
}

// MIME type is carried through per item so the caller can decode the blob in
// the right context.
func TestContentBatch_CarriesMimeType(t *testing.T) {
	id := uuid.New()
	rr := runContentBatch(t, `{"ids":["`+id.String()+`"]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{
			id: {ID: id, ExternalID: "ext", MimeType: "application/x-yjs-snapshot"},
		}
		storage.blobsByExternalID = map[string][]byte{"ext": []byte("data")}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].MimeType != "application/x-yjs-snapshot" {
		t.Errorf("mimeType = %q, want application/x-yjs-snapshot", resp.Items[0].MimeType)
	}
}

// Order MUST be preserved: items[i] corresponds to the i-th requested id, even
// when ids repeat and even when a duplicate maps to the same row. The caller
// relies on positional correspondence (no per-item id matching) for its
// resolver fan-in.
func TestContentBatch_PreservesOrder(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	// Deliberately request out of "natural" order, with a repeat (a appears twice).
	order := []uuid.UUID{c, a, b, a}
	quoted := make([]string, len(order))
	for i, id := range order {
		quoted[i] = `"` + id.String() + `"`
	}

	rr := runContentBatch(t, `{"ids":[`+strings.Join(quoted, ",")+`]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{
			a: {ID: a, ExternalID: "ext-a", MimeType: "text/plain"},
			b: {ID: b, ExternalID: "ext-b", MimeType: "text/plain"},
			c: {ID: c, ExternalID: "ext-c", MimeType: "text/plain"},
		}
		storage.blobsByExternalID = map[string][]byte{
			"ext-a": []byte("AAA"),
			"ext-b": []byte("BBB"),
			"ext-c": []byte("CCC"),
		}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != len(order) {
		t.Fatalf("len(items) = %d, want %d (one per requested id, duplicates preserved)", len(resp.Items), len(order))
	}
	for i, item := range resp.Items {
		wantID := order[i]
		if item.ID != wantID.String() {
			t.Errorf("items[%d].id = %s, want %s (order not preserved)", i, item.ID, wantID)
		}
	}
}

// Missing ids are reported non-fatally: an unknown document id (or one whose
// blob is gone from storage) yields found=false for that item while the rest
// of the batch still resolves. The endpoint never 404s for a partial miss —
// that is the whole point of a batch read feeding a best-effort resolver.
func TestContentBatch_MissingReportedNonFatally(t *testing.T) {
	present := uuid.New()
	unknownDoc := uuid.New()  // no repo row
	blobMissing := uuid.New() // repo row exists, blob absent from storage
	order := []uuid.UUID{present, unknownDoc, blobMissing}
	quoted := make([]string, len(order))
	for i, id := range order {
		quoted[i] = `"` + id.String() + `"`
	}

	rr := runContentBatch(t, `{"ids":[`+strings.Join(quoted, ",")+`]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{
			present:     {ID: present, ExternalID: "ext-present", MimeType: "text/plain"},
			blobMissing: {ID: blobMissing, ExternalID: "ext-gone", MimeType: "text/plain"},
			// unknownDoc intentionally absent → GetByID returns ErrDocumentNotFound.
		}
		storage.blobsByExternalID = map[string][]byte{
			"ext-present": []byte("here"),
			// "ext-gone" intentionally absent → Read returns os.ErrNotExist.
		}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial miss is non-fatal), body: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(resp.Items))
	}

	if !resp.Items[0].Found || resp.Items[0].ContentBase64 == "" {
		t.Errorf("items[0] (present) found=%v content=%q, want found + content", resp.Items[0].Found, resp.Items[0].ContentBase64)
	}
	for i, label := range map[int]string{1: "unknown-doc", 2: "blob-missing"} {
		if resp.Items[i].Found {
			t.Errorf("items[%d] (%s) found = true, want false", i, label)
		}
		if resp.Items[i].Error == "" {
			t.Errorf("items[%d] (%s) error empty, want a non-fatal reason", i, label)
		}
		if resp.Items[i].ContentBase64 != "" {
			t.Errorf("items[%d] (%s) content non-empty on a miss", i, label)
		}
	}
}

// A syntactically invalid id inside the batch is reported as a non-fatal miss
// for that position — the well-formed ids still resolve. The endpoint does not
// reject the whole request for one bad id.
func TestContentBatch_InvalidIDInBatchNonFatal(t *testing.T) {
	good := uuid.New()
	rr := runContentBatch(t, `{"ids":["`+good.String()+`","not-a-uuid"]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{
			good: {ID: good, ExternalID: "ext", MimeType: "text/plain"},
		}
		storage.blobsByExternalID = map[string][]byte{"ext": []byte("ok")}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Items))
	}
	if !resp.Items[0].Found {
		t.Errorf("items[0] (valid id) found = false, want true")
	}
	if resp.Items[0].ID != good.String() {
		t.Errorf("items[0].id = %s, want %s", resp.Items[0].ID, good)
	}
	if resp.Items[1].Found {
		t.Errorf("items[1] (invalid id) found = true, want false")
	}
	if resp.Items[1].ID != "not-a-uuid" {
		t.Errorf("items[1].id = %q, want the malformed id echoed back", resp.Items[1].ID)
	}
	if resp.Items[1].Error == "" {
		t.Errorf("items[1] (invalid id) error empty, want a reason")
	}
}

// A single id is a valid batch of one — the endpoint behaves identically to
// the single-read path it reuses.
func TestContentBatch_SingleID(t *testing.T) {
	id := uuid.New()
	rr := runContentBatch(t, `{"ids":["`+id.String()+`"]}`, func(repo *mockDocRepo, storage *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{id: {ID: id, ExternalID: "ext", MimeType: "text/plain"}}
		storage.blobsByExternalID = map[string][]byte{"ext": []byte("solo")}
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != 1 || !resp.Items[0].Found {
		t.Fatalf("items = %+v, want one found item", resp.Items)
	}
	decoded, _ := base64.StdEncoding.DecodeString(resp.Items[0].ContentBase64)
	if string(decoded) != "solo" {
		t.Errorf("content = %q, want %q", decoded, "solo")
	}
}

// An empty ids array is a client error (nothing to read) → 400, distinguishing
// "you asked for nothing" from "everything you asked for was missing" (200 with
// found=false items).
func TestContentBatch_EmptyIDs_400(t *testing.T) {
	rr := runContentBatch(t, `{"ids":[]}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty ids", rr.Code)
	}
}

// A missing ids field is the same client error as an empty array.
func TestContentBatch_OmittedIDs_400(t *testing.T) {
	rr := runContentBatch(t, `{}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for omitted ids", rr.Code)
	}
}

// The batch size is bounded to protect the service from an unbounded fan-out of
// per-id DB+storage reads in a single request; over the cap → 400.
func TestContentBatch_TooManyIDs_400(t *testing.T) {
	ids := make([]string, maxContentBatchSize+1)
	for i := range ids {
		ids[i] = `"` + uuid.New().String() + `"`
	}
	rr := runContentBatch(t, `{"ids":[`+strings.Join(ids, ",")+`]}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for over-cap batch", rr.Code)
	}
}

// Exactly the cap is accepted (boundary).
func TestContentBatch_AtCap_OK(t *testing.T) {
	ids := make([]string, maxContentBatchSize)
	for i := range ids {
		ids[i] = `"` + uuid.New().String() + `"`
	}
	// No docs seeded → every item resolves as a non-fatal miss, but the
	// request itself is accepted (200), which is what this boundary asserts.
	rr := runContentBatch(t, `{"ids":[`+strings.Join(ids, ",")+`]}`, func(repo *mockDocRepo, _ *mockStorage) {
		repo.docsByID = map[uuid.UUID]model.Document{} // empty, id-aware → all miss
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 at exactly the cap, body: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatchResponse(t, rr)
	if len(resp.Items) != maxContentBatchSize {
		t.Fatalf("len(items) = %d, want %d", len(resp.Items), maxContentBatchSize)
	}
}

// Malformed JSON body → 400.
func TestContentBatch_MalformedJSON_400(t *testing.T) {
	rr := runContentBatch(t, `{"ids":[`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed JSON", rr.Code)
	}
}

// Unknown fields in the body → 400 (strict decode), matching the other
// JSON-bodied internal endpoints (Copy, Update).
func TestContentBatch_UnknownField_400(t *testing.T) {
	id := uuid.New()
	rr := runContentBatch(t, `{"ids":["`+id.String()+`"],"surprise":true}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown field", rr.Code)
	}
}
