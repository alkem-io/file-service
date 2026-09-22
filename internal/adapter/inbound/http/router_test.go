package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/service"
)

func testRouter() http.Handler {
	logger, _ := zap.NewDevelopment()

	repo := &mockDocRepo{doc: model.Document{
		ID:              uuid.New(),
		ExternalID:      "abc",
		MimeType:        "text/plain",
		AuthorizationID: uuid.New(),
		StorageBucketID: uuid.New(),
	}}
	// Scripted with a DISTINCT externalID so a by-reference resolution is
	// identifiable in the response body — the route fence below asserts the
	// request actually reached ByReference, not merely "not a 404".
	byReference := "media_id_router"
	repo.refDoc = &model.Document{
		ID:                uuid.New(),
		ExternalID:        "by-reference-hash",
		MimeType:          "text/plain",
		AuthorizationID:   uuid.New(),
		StorageBucketID:   uuid.New(),
		ExternalReference: &byReference,
	}
	storage := &mockStorage{data: []byte("content")}
	svc := &service.FileService{
		Repo:      repo,
		Auth:      &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage:   storage,
		Processor: &stubProcessor{},
	}

	return NewRouter(Deps{
		PublicHandler: &PublicHandler{
			Repo:    repo,
			Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
			Storage: storage,
			MaxAge:  86400,
		},
		DocumentHandler: &DocumentHandler{Service: svc, MaxAge: 86400},
		HealthHandler: &HealthHandler{
			DB:   &mockPinger{},
			NATS: &mockConnChecker{connected: true},
		},
		Logger: logger,
	})
}

func TestRouter_HealthEndpoint(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", rr.Code)
	}
}

// /live is a process-alive check for K8s livenessProbe. It must return 200
// regardless of DB/NATS state — the point is to detect deadlocked pods, not
// dependency failures.
func TestRouter_LiveEndpoint(t *testing.T) {
	t.Run("HealthyDeps", func(t *testing.T) {
		r := testRouter()
		req := httptest.NewRequest(http.MethodGet, "/live", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("GET /live = %d, want 200", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	})

	// Critical invariant: /live MUST return 200 even when readiness dependencies
	// are broken. Liveness = process alive; dependency health belongs to /health.
	t.Run("BrokenDeps", func(t *testing.T) {
		logger, _ := zap.NewDevelopment()
		r := NewRouter(Deps{
			PublicHandler:   &PublicHandler{},
			DocumentHandler: &DocumentHandler{},
			HealthHandler: &HealthHandler{
				DB:   &mockPinger{err: errors.New("db down")},
				NATS: &mockConnChecker{connected: false},
			},
			Logger: logger,
		})

		req := httptest.NewRequest(http.MethodGet, "/live", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("GET /live with broken deps = %d, want 200 (liveness must not depend on readiness)", rr.Code)
		}
	})
}

func TestRouter_DebugVars(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodGet, "/internal/debug/vars", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /internal/debug/vars = %d, want 200", rr.Code)
	}
}

// Anonymous reach-through: the gateway ALWAYS stamps X-Alkemio-Actor-Id —
// anonymous callers get the nil-UUID sentinel, which auth-evaluation-service
// resolves to GLOBAL_ANONYMOUS. The public route must serve such requests
// (policy decides), but 401 when the header is absent entirely — that means
// the gateway never ran and the request bypassed the trust chain.
// testRouter() seeds a doc and a permissive mockAuth, so anonymous → 200.
func TestRouter_PublicDocument_AcceptsAnonymous(t *testing.T) {
	t.Run("GatewayStampedAnonymous", func(t *testing.T) {
		r := testRouter()

		req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
		req.Header.Set(HeaderActorID, uuid.Nil.String())
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("GET /rest/storage/document/:id with nil-UUID actor header = %d, want 200 (auth-eval allows anonymous via testRouter mock)", rr.Code)
		}
	})

	t.Run("MissingHeader_401", func(t *testing.T) {
		r := testRouter()

		req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET /rest/storage/document/:id without actor header = %d, want 401 (gateway didn't stamp identity)", rr.Code)
		}
	})
}

func TestRouter_InternalGetMeta(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Should not be route-level 404 (route exists) — handler-level failures are acceptable
	if rr.Code == http.StatusNotFound {
		t.Errorf("GET /internal/file/:id/meta returned route-level 404")
	}
}

func TestRouter_InternalGetContent(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed {
		t.Error("route not registered for GET /internal/file/:id/content")
	}
}

func TestRouter_InternalCreateDocument(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodPost, "/internal/file", strings.NewReader(""))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// 400 (bad request) is fine — route is registered, handler rejects invalid input
	if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed {
		t.Errorf("POST /internal/file = %d, route not registered", rr.Code)
	}
}

func TestRouter_InternalDeleteDocument(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodDelete, "/internal/file/"+uuid.New().String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed {
		t.Error("route not registered for DELETE /internal/file/:id")
	}
}

// POST /internal/file/content-batch must be registered under the same internal
// (no-auth) route group as the other /internal/file endpoints. A well-formed
// batch resolves to 200 through the full router (global middleware + no actor
// header), confirming the wiring — not just that the route exists.
func TestRouter_InternalContentBatch(t *testing.T) {
	r := testRouter()

	body := `{"ids":["` + uuid.New().String() + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/internal/file/content-batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed {
		t.Fatalf("POST /internal/file/content-batch = %d, route not registered", rr.Code)
	}
	// testRouter seeds a doc + blob and no auth on /internal, so a well-formed
	// batch returns 200 (the single seeded row resolves; the point is the
	// internal middleware chain let it through without an actor header).
	if rr.Code != http.StatusOK {
		t.Errorf("POST /internal/file/content-batch = %d, want 200 (no-auth internal route)", rr.Code)
	}
}

func TestRouter_InternalPatchDocument(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodPatch, "/internal/file/"+uuid.New().String(), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed {
		t.Error("route not registered for PATCH /internal/file/:id")
	}
}

func TestRouter_InternalPutContent(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodPut, "/internal/file/"+uuid.New().String()+"/content", strings.NewReader("data"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusMethodNotAllowed {
		t.Error("route not registered for PUT /internal/file/:id/content")
	}
}

func TestRouter_InternalNoAuth(t *testing.T) {
	r := testRouter()

	// Internal endpoints should work without Authorization header
	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Should NOT be 401 — internal routes have no JWT middleware
	if rr.Code == http.StatusUnauthorized {
		t.Error("internal route should not require auth")
	}
}

// TestRouter_StaticAndParameterizedSiblingsCoexist guards the genuinely risky thing about the
// /internal/file subtree: static segments (`/file/copy`) live alongside the parameterized
// /file/{id}/meta, so neither may shadow the other — an assumption about the router, therefore
// asserted rather than assumed.
//
// Each route is identified by BEHAVIOUR, not just by "not 404": the body carrying an unknown
// `ids` field is sent to /file/copy, where Copy's strict decode must reject it.
func TestRouter_StaticAndParameterizedSiblingsCoexist(t *testing.T) {
	batchBody := `{"ids":["` + uuid.New().String() + `"]}`

	// The parameterized sibling must still resolve — a literal segment registered next to
	// {id} must not shadow it.
	t.Run("SingleMetaStillRoutes", func(t *testing.T) {
		r := testRouter()
		req := httptest.NewRequest(http.MethodGet, "/internal/file/"+uuid.New().String()+"/meta", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("GET /internal/file/{id}/meta = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"externalID"`) {
			t.Errorf("body = %s, want the single-document meta shape", rr.Body.String())
		}
	})

	// The static POST sibling must still reach Copy — proven by Copy REJECTING the batch body.
	t.Run("CopyStillRoutesToCopy", func(t *testing.T) {
		r := testRouter()
		req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(batchBody))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("POST /internal/file/copy with a batch body = %d, want 400 (Copy rejects the unknown `ids` field): %s",
				rr.Code, rr.Body.String())
		}
	})
}

// TestRouter_ByReferenceEndpoint guards that GET /internal/file/by-reference is actually
// wired in NewRouter. It is a STATIC segment sharing a subtree with the parameterized
// /internal/file/{id}/... routes, so "not a 404" proves nothing on its own: with the route
// line deleted, chi answers from a neighbouring node and the whole suite stayed green.
//
// Both sub-cases are therefore BEHAVIOURAL — only ByReference itself can produce them.
func TestRouter_ByReferenceEndpoint(t *testing.T) {
	// A resolved lookup returns the document THIS repo scripts only for the
	// by-reference variant, so the body proves which handler answered.
	t.Run("ResolvesThroughByReference", func(t *testing.T) {
		r := testRouter()
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=media_id_router", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("GET /internal/file/by-reference = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"externalID":"by-reference-hash"`) {
			t.Errorf("body = %s, want the by-reference document (some other route answered)", rr.Body.String())
		}
	})

	// The missing-ref 400 is unique to this handler: no neighbouring route can
	// answer a GET on this path with a 400 (a router-level miss is 404/405), and
	// the parameterized sibling would treat "by-reference" as a document id.
	t.Run("MissingRefIs400FromByReference", func(t *testing.T) {
		r := testRouter()
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("GET /internal/file/by-reference without ref = %d, want 400: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestRouter_BlobContentEndpoint guards that GET /internal/blob/{hash}/content is actually
// wired in NewRouter (not just reachable via a hand-rolled test router). testRouter's
// mockStorage serves its content for any key, so a registered route returns 200; if the
// route line is dropped in a rebase or {hash} is renamed, chi returns 404 and this fails —
// the same registration guard the other internal routes have.
func TestRouter_BlobContentEndpoint(t *testing.T) {
	r := testRouter()
	const hash = "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a"
	req := httptest.NewRequest(http.MethodGet, "/internal/blob/"+hash+"/content", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code == http.StatusNotFound {
		t.Fatal("GET /internal/blob/{hash}/content is not registered in NewRouter (404)")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "content" {
		t.Fatalf("body = %q, want %q", got, "content")
	}
}
