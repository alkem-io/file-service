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

// JWTExtractor is a permissive parser, not a gatekeeper. Anonymous requests
// (no header, malformed token, missing claim) flow through with empty actorID;
// the auth-evaluation-service makes the policy decision downstream.
func TestJWTMiddleware_AnonymousCases(t *testing.T) {
	cases := []struct {
		name      string
		setHeader bool
		headerVal string
	}{
		{"NoAuthHeader", false, ""},
		{"NonBearerScheme", true, "Basic dXNlcjpwYXNz"},
		{"BearerNotJWT", true, "Bearer not-a-jwt"},
		{"BearerMalformedBase64", true, "Bearer header.NOT_BASE64!.sig"},
		{"BearerNoActorClaim", true, "Bearer " + fakeJWT(t, map[string]any{"sub": "some-user"})},
		{"BearerEmptyActorClaim", true, "Bearer " + fakeJWT(t, map[string]any{"alkemio_actor_id": ""})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.setHeader {
				req.Header.Set("Authorization", tc.headerVal)
			}

			var (
				handlerCalled bool
				gotActorID    string
			)
			handler := JWTExtractor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlerCalled = true
				gotActorID = GetActorID(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if !handlerCalled {
				t.Fatal("handler must be called for anonymous requests; middleware must not 401")
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (middleware passes through)", rr.Code)
			}
			if gotActorID != "" {
				t.Errorf("actorID = %q, want empty for anonymous", gotActorID)
			}
		})
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
