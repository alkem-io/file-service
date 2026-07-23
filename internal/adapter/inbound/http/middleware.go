package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type contextKey string

const (
	ctxKeyRequestID contextKey = "requestID"
	ctxKeyActorID   contextKey = "actorID"
)

// RequestID generates a unique request ID and adds it to the context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID extracts the request ID from context.
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// GetActorID extracts the actor ID from context (set by JWT middleware).
func GetActorID(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyActorID).(string); ok {
		return id
	}
	return ""
}

// HeaderActorID is the header carrying the gateway-resolved actor identity.
const HeaderActorID = "X-Alkemio-Actor-Id"

// ActorHeaderExtractor reads the actor id from the `X-Alkemio-Actor-Id`
// header stamped by Traefik's `alkemio-resolve` forwardAuth middleware
// (alkemio-server's /api/auth/resolve endpoint). The gateway is responsible
// for validating the request's credentials (cookie session OR Hydra-issued
// bearer) before stamping the header. The chain `strip-client-alkemio-headers`
// → `alkemio-resolve` ensures client-supplied X-Alkemio-* are blanked before
// resolve runs, so the header file-service receives is server-trusted.
//
// The gateway ALWAYS stamps the header: anonymous callers get the nil-UUID
// sentinel, which auth-evaluation-service resolves to GLOBAL_ANONYMOUS.
// 401 if the header is missing — the gateway didn't run for this request.
func ActorHeaderExtractor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actorID := r.Header.Get(HeaderActorID)
		if actorID == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing actor identity")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyActorID, actorID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// JWTExtractor extracts the alkemio_actor_id claim from the Oathkeeper-injected JWT.
// Oathkeeper has already validated the token; we just decode the payload.
//
// Permissive by design: never 401s. Anonymous requests (no Authorization
// header, missing claim, even malformed token) flow through with an empty
// actorID. Authorization decisions belong to the auth-evaluation-service,
// which evaluates the resource's policy against the (possibly anonymous)
// caller. This unblocks the public file-serve route, which must serve
// public-bucket visuals to non-authenticated users — those documents'
// authorization policies grant `read` to the `global-anonymous` credential
// that every caller implicitly carries.
//
// DEPRECATED post-Oathkeeper-removal: use `ActorHeaderExtractor` once the
// `alkemio-resolve` gateway forwardAuth is in place. Kept for backwards-compat
// during the migration window.
func JWTExtractor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actorID := extractActorID(r.Header.Get("Authorization"))
		ctx := context.WithValue(r.Context(), ctxKeyActorID, actorID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractActorID parses an Authorization header and returns the
// alkemio_actor_id claim if present and non-empty. Any failure to extract
// (missing header, wrong scheme, malformed JWT, missing/empty claim)
// returns "" — anonymous. The function never errors; the auth-evaluation
// service is the policy decision point, not this parser.
func extractActorID(auth string) string {
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if id, ok := claims["alkemio_actor_id"].(string); ok {
		return id
	}
	return ""
}

// RequestLogger logs request start/end with duration.
// Health-probe endpoints (/live, /health) are excluded from logs: they are hit
// by Kubernetes probes every few seconds and would dominate the log stream.
func RequestLogger(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(ww, r)

			if r.Method == http.MethodGet && (r.URL.Path == "/live" || r.URL.Path == "/health") {
				return
			}

			logger.Info("request",
				zap.String("requestID", GetRequestID(r.Context())),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", ww.status),
				zap.Duration("duration", time.Since(start)),
			)
		})
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code for the
// request log. Defaults to 200 because handlers that only Write never call
// WriteHeader explicitly.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status code before delegating to the wrapped
// ResponseWriter.
func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap keeps optional transport features reachable through the request-logging wrapper,
// including http.ResponseController's read/write deadline support.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
