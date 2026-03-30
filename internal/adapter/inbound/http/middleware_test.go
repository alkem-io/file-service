package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestJWTMiddleware_ValidJWT(t *testing.T) {
	payload := map[string]any{"alkemio_actor_id": "actor-uuid-123"}
	token := fakeJWT(t, payload)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	var gotActorID string
	handler := JWTExtractor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActorID = GetActorID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotActorID != "actor-uuid-123" {
		t.Errorf("actorID = %q, want %q", gotActorID, "actor-uuid-123")
	}
}

func TestJWTMiddleware_MissingAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	handler := JWTExtractor(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestJWTMiddleware_InvalidToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")

	handler := JWTExtractor(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestJWTMiddleware_MissingActorClaim(t *testing.T) {
	payload := map[string]any{"sub": "some-user"}
	token := fakeJWT(t, payload)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	handler := JWTExtractor(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler should not be called")
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestRequestID_SetsHeaderAndContext(t *testing.T) {
	var gotRequestID string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestID = GetRequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if gotRequestID == "" {
		t.Error("expected non-empty request ID in context")
	}
	if rr.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header")
	}
	if rr.Header().Get("X-Request-ID") != gotRequestID {
		t.Error("header and context request IDs should match")
	}
}

func TestGetRequestID_EmptyContext(t *testing.T) {
	got := GetRequestID(context.Background())
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestRequestLogger_LogsRequest(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	handler := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestRequestLogger_CapturesStatusCode(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	handler := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestGetActorID_EmptyContext(t *testing.T) {
	got := GetActorID(context.Background())
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// fakeJWT builds a minimal unsigned JWT with the given payload claims.
// Oathkeeper already validated the token; we just need to extract claims.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + encodedPayload + "."
}
