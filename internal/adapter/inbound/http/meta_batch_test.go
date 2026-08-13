package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// runMetaBatch dispatches one POST /internal/file/meta-batch through a chi
// router carrying the LITERAL route pattern the real router registers, so the
// tests exercise the same path string production does. configure seeds the repo
// mock before the request is served.
func runMetaBatch(t *testing.T, body string, configure func(*mockDocRepo)) (*httptest.ResponseRecorder, *mockDocRepo) {
	t.Helper()
	h, repo, _ := newDocHandler()
	if configure != nil {
		configure(repo)
	}
	r := chi.NewRouter()
	r.Post("/internal/file/meta-batch", h.GetMetaBatch)

	req := httptest.NewRequest(http.MethodPost, "/internal/file/meta-batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr, repo
}

// batchBody renders the request body for a set of ids.
func batchBody(ids ...string) string {
	raw, err := json.Marshal(DocumentMetaBatchRequest{IDs: ids})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// sampleDoc is a fully-populated row: every optional field set, so a shape
// regression on ANY of them is visible.
func sampleDoc(id uuid.UUID, name string) model.Document {
	createdBy := uuid.New()
	tagset := uuid.New()
	ref := "media_" + name
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return model.Document{
		ID:                id,
		ExternalID:        "hash_" + name,
		MimeType:          "image/png",
		Size:              1234,
		DisplayName:       name + ".png",
		CreatedBy:         &createdBy,
		TemporaryLocation: false,
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		TagsetID:          &tagset,
		ExternalReference: &ref,
		CreatedDate:       created,
		UpdatedDate:       created.Add(time.Hour),
		ImageWidth:        intp(640),
		ImageHeight:       intp(480),
	}
}

// decodeBatch parses a 200 batch body into its files, keyed by id.
func decodeBatch(t *testing.T, rr *httptest.ResponseRecorder) map[string]map[string]any {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	if body.Files == nil {
		t.Fatalf("`files` key missing or null; the caller maps by id and must always get an array: %s", rr.Body.String())
	}
	out := make(map[string]map[string]any, len(body.Files))
	for _, f := range body.Files {
		id, _ := f["id"].(string)
		if _, dup := out[id]; dup {
			t.Fatalf("document %s returned more than once: %s", id, rr.Body.String())
		}
		out[id] = f
	}
	return out
}

// singleMetaJSON fetches the same document through GET /internal/file/{id}/meta
// and returns its decoded body — the reference shape the batch must match.
func singleMetaJSON(t *testing.T, doc model.Document) map[string]any {
	t.Helper()
	h, repo, _ := newDocHandler()
	repo.doc = doc
	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)
	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+doc.ID.String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("single /meta status = %d, want 200", rr.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The happy path, asserted against the SINGLE-document endpoint rather than a
// hand-written expectation: the contract is that a batch element is byte-for-
// byte the shape GET /meta already returns. Comparing to a literal here would
// let the two drift while both tests stayed green; comparing to the live single
// endpoint is what makes "reuse the DTO, don't define a parallel one" testable.
func TestMetaBatch_HappyPath_ShapeMatchesSingleMeta(t *testing.T) {
	a, b, c := sampleDoc(uuid.New(), "alpha"), sampleDoc(uuid.New(), "beta"), sampleDoc(uuid.New(), "gamma")

	rr, repo := runMetaBatch(t, batchBody(a.ID.String(), b.ID.String(), c.ID.String()), func(repo *mockDocRepo) {
		repo.batchDocs = map[uuid.UUID]model.Document{a.ID: a, b.ID: b, c.ID: c}
	})

	got := decodeBatch(t, rr)
	if len(got) != 3 {
		t.Fatalf("files = %d, want 3: %s", len(got), rr.Body.String())
	}
	for _, doc := range []model.Document{a, b, c} {
		want := singleMetaJSON(t, doc)
		if !reflect.DeepEqual(got[doc.ID.String()], want) {
			t.Errorf("batch element for %s diverges from GET /meta\n got: %#v\nwant: %#v", doc.ID, got[doc.ID.String()], want)
		}
	}
	if repo.batchCalls != 1 {
		t.Errorf("GetByIDs called %d×, want 1", repo.batchCalls)
	}
}

// A partial result is NORMAL: a document deleted between the caller's read and
// this batch must not fail the request. Unresolved ids are omitted; the status
// stays 200.
func TestMetaBatch_PartialResolutionIs200(t *testing.T) {
	found, alsoFound := sampleDoc(uuid.New(), "found"), sampleDoc(uuid.New(), "found2")
	missing, alsoMissing := uuid.New(), uuid.New()

	rr, _ := runMetaBatch(t,
		batchBody(found.ID.String(), missing.String(), alsoFound.ID.String(), alsoMissing.String()),
		func(repo *mockDocRepo) {
			repo.batchDocs = map[uuid.UUID]model.Document{found.ID: found, alsoFound.ID: alsoFound}
		})

	got := decodeBatch(t, rr)
	if len(got) != 2 {
		t.Fatalf("files = %d, want 2 (the resolvable subset): %s", len(got), rr.Body.String())
	}
	for _, id := range []uuid.UUID{found.ID, alsoFound.ID} {
		if _, ok := got[id.String()]; !ok {
			t.Errorf("resolvable id %s missing from files", id)
		}
	}
	for _, id := range []uuid.UUID{missing, alsoMissing} {
		if _, ok := got[id.String()]; ok {
			t.Errorf("unresolvable id %s present in files", id)
		}
	}
}

// Every id unresolvable is still a 200 with an EMPTY array — not a 404. The
// caller maps by id and treats absence as "gone", which must not be conflated
// with a transport/lookup failure.
func TestMetaBatch_NoneResolveIs200EmptyArray(t *testing.T) {
	rr, _ := runMetaBatch(t, batchBody(uuid.New().String(), uuid.New().String()), nil)

	got := decodeBatch(t, rr)
	if len(got) != 0 {
		t.Fatalf("files = %d, want 0", len(got))
	}
	if !strings.Contains(rr.Body.String(), `"files":[]`) {
		t.Errorf("body = %s, want an empty `files` ARRAY (never null)", rr.Body.String())
	}
}

// Duplicate ids are de-duplicated: the document comes back once, and the repo
// is asked for it once — a caller repeating an id must not multiply the work.
func TestMetaBatch_DuplicateIDsReturnedOnce(t *testing.T) {
	doc := sampleDoc(uuid.New(), "dup")
	other := sampleDoc(uuid.New(), "other")
	id := doc.ID.String()

	rr, repo := runMetaBatch(t, batchBody(id, other.ID.String(), id, id), func(repo *mockDocRepo) {
		repo.batchDocs = map[uuid.UUID]model.Document{doc.ID: doc, other.ID: other}
	})

	// decodeBatch fails the test outright on a repeated id.
	got := decodeBatch(t, rr)
	if len(got) != 2 {
		t.Fatalf("files = %d, want 2 (each document at most once): %s", len(got), rr.Body.String())
	}
	if len(repo.lastBatchIDs) != 2 {
		t.Errorf("repo received %d ids (%v), want 2 — duplicates must be collapsed before the query",
			len(repo.lastBatchIDs), repo.lastBatchIDs)
	}
	if repo.batchCalls != 1 {
		t.Errorf("GetByIDs called %d×, want 1", repo.batchCalls)
	}
}

// The bound is the endpoint's protection against a caller simply moving its
// unbounded fan-out across the wire. 100 is accepted; 101 is refused BEFORE any
// repo call — the request is rejected, not partially served.
func TestMetaBatch_CapIsEnforcedAt100(t *testing.T) {
	ids := make([]string, 0, maxMetaBatchIDs+1)
	for range maxMetaBatchIDs + 1 {
		ids = append(ids, uuid.New().String())
	}

	t.Run("AtCap", func(t *testing.T) {
		rr, repo := runMetaBatch(t, batchBody(ids[:maxMetaBatchIDs]...), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for exactly %d ids: %s", rr.Code, maxMetaBatchIDs, rr.Body.String())
		}
		if repo.batchCalls != 1 {
			t.Errorf("GetByIDs called %d×, want 1", repo.batchCalls)
		}
	})

	t.Run("OverCap", func(t *testing.T) {
		rr, repo := runMetaBatch(t, batchBody(ids...), nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for %d ids: %s", rr.Code, len(ids), rr.Body.String())
		}
		if repo.batchCalls != 0 {
			t.Errorf("GetByIDs called %d× on an over-cap request; it must be rejected before the query", repo.batchCalls)
		}
	})
}

// A malformed element is a 400, never a silent skip: skipping would be
// indistinguishable to the caller from "that document is gone".
func TestMetaBatch_NonUUIDElementIs400(t *testing.T) {
	valid := sampleDoc(uuid.New(), "valid")

	for _, bad := range []string{"not-a-uuid", "", "   ", "12345"} {
		t.Run("element="+bad, func(t *testing.T) {
			rr, repo := runMetaBatch(t, batchBody(valid.ID.String(), bad), func(repo *mockDocRepo) {
				repo.batchDocs = map[uuid.UUID]model.Document{valid.ID: valid}
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for element %q: %s", rr.Code, bad, rr.Body.String())
			}
			if repo.batchCalls != 0 {
				t.Errorf("GetByIDs called %d× despite a malformed element", repo.batchCalls)
			}
		})
	}
}

// Body-level rejections. decodeStrictJSON owns the JSON rules (1 MiB cap,
// DisallowUnknownFields, no trailing data); the handler owns "ids must be a
// non-empty array". Both must surface as 400 with no repo call.
func TestMetaBatch_MalformedBodyIs400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"MalformedJSON", `{"ids": [`},
		{"NotAnObject", `["` + uuid.New().String() + `"]`},
		{"TrailingData", `{"ids":["` + uuid.New().String() + `"]}{"ids":[]}`},
		{"UnknownField", `{"ids":["` + uuid.New().String() + `"],"bucketId":"x"}`},
		{"WrongElementType", `{"ids":[123]}`},
		{"EmptyList", `{"ids":[]}`},
		{"NullList", `{"ids":null}`},
		{"MissingIDsKey", `{}`},
		{"EmptyBody", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr, repo := runMetaBatch(t, tc.body, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
			}
			if repo.batchCalls != 0 {
				t.Errorf("GetByIDs called %d× on a malformed body", repo.batchCalls)
			}
		})
	}
}

// A staging document (matrix_media store) has a NULL authorizationId column,
// which reads back on the Document as the ZERO UUID. The batch must omit the
// field exactly as the single /meta does — emitting
// "00000000-0000-0000-0000-000000000000" would report an UNAUTHORIZED document
// as authorized under an all-zero policy.
func TestMetaBatch_PolicylessDocumentOmitsAuthorizationID(t *testing.T) {
	doc := sampleDoc(uuid.New(), "staging")
	doc.AuthorizationID = uuid.Nil
	doc.CreatedBy = nil
	doc.TagsetID = nil
	doc.ExternalReference = nil

	rr, _ := runMetaBatch(t, batchBody(doc.ID.String()), func(repo *mockDocRepo) {
		repo.batchDocs = map[uuid.UUID]model.Document{doc.ID: doc}
	})

	got := decodeBatch(t, rr)[doc.ID.String()]
	for _, field := range []string{"authorizationId", "createdBy", "tagsetId", "externalReference"} {
		if v, present := got[field]; present {
			t.Errorf("%s present (= %v) on a policy-less document; it must be OMITTED", field, v)
		}
	}
	if !reflect.DeepEqual(got, singleMetaJSON(t, doc)) {
		t.Errorf("policy-less batch element diverges from GET /meta\n got: %#v\nwant: %#v", got, singleMetaJSON(t, doc))
	}
}

// THE reason this endpoint exists: N ids cost ONE repo round-trip. A handler
// that looped GetByID would answer identically on every other test in this
// file — this is the only one that can tell the difference.
func TestMetaBatch_OneRepoRoundTripForManyIDs(t *testing.T) {
	const n = 50
	docs := make(map[uuid.UUID]model.Document, n)
	ids := make([]string, 0, n)
	for i := range n {
		d := sampleDoc(uuid.New(), string(rune('a'+i%26)))
		docs[d.ID] = d
		ids = append(ids, d.ID.String())
	}

	rr, repo := runMetaBatch(t, batchBody(ids...), func(repo *mockDocRepo) {
		repo.batchDocs = docs
	})

	if got := decodeBatch(t, rr); len(got) != n {
		t.Fatalf("files = %d, want %d", len(got), n)
	}
	if repo.batchCalls != 1 {
		t.Errorf("GetByIDs called %d× for %d ids, want exactly 1 — batching is the endpoint's entire purpose", repo.batchCalls, n)
	}
	if repo.getByIDCalls != 0 {
		t.Errorf("single-document GetByID called %d×; the batch must never fan out per id", repo.getByIDCalls)
	}
	if len(repo.lastBatchIDs) != n {
		t.Errorf("repo received %d ids, want %d", len(repo.lastBatchIDs), n)
	}
}

// A repo failure is a logged 500, not a partial 200: the caller must be able to
// tell "these documents are gone" (200, omitted) from "the lookup failed"
// (500) — collapsing the two would silently blank out real metadata.
func TestMetaBatch_RepoErrorIs500(t *testing.T) {
	rr, _ := runMetaBatch(t, batchBody(uuid.New().String()), func(repo *mockDocRepo) {
		repo.batchErr = errors.New("connection refused")
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rr.Code, rr.Body.String())
	}
}
