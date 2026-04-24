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

// JWTExtractor extracts the alkemio_actor_id claim from the Oathkeeper-injected JWT.
// Oathkeeper has already validated the token; we just decode the payload.
func JWTExtractor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid authorization header")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		parts := strings.Split(token, ".")
		if len(parts) < 2 {
			writeJSONError(w, http.StatusUnauthorized, "invalid token format")
			return
		}

		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid token encoding")
			return
		}

		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid token payload")
			return
		}

		actorID, ok := claims["alkemio_actor_id"].(string)
		if !ok || actorID == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing alkemio_actor_id claim")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyActorID, actorID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

			if r.URL.Path == "/live" || r.URL.Path == "/health" {
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

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
