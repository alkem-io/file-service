package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/domain/model"
	"github.com/alkem-io/file-service-go/internal/domain/service"
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

func TestRouter_PublicDocumentRequiresJWT(t *testing.T) {
	r := testRouter()

	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+uuid.New().String(), nil)
	// No Authorization header
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /rest/storage/document/:id without JWT = %d, want 401", rr.Code)
	}
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
