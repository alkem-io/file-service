package http

import (
	"context"
	"net/http"
	"time"
)

// Pinger can check if a connection is alive.
type Pinger interface {
	// Ping performs an active round-trip to the dependency (pgxpool.Pool
	// satisfies this directly). The handler calls it under a 2-second
	// timeout, so implementations must honor ctx cancellation.
	Ping(ctx context.Context) error
}

// ConnectionChecker can report if it's connected.
type ConnectionChecker interface {
	// IsConnected reports the connection's last-known state without
	// performing I/O — nats.Conn satisfies this directly.
	IsConnected() bool
}

// HealthHandler handles GET /health.
type HealthHandler struct {
	DB   Pinger
	NATS ConnectionChecker
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	details := map[string]string{}
	healthy := true

	// Check DB
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.DB.Ping(ctx); err != nil {
		details["database"] = "unreachable"
		healthy = false
	} else {
		details["database"] = "ok"
	}

	// Check NATS (only when configured — nil means h2c transport, NATS not used)
	if h.NATS != nil {
		if !h.NATS.IsConnected() {
			details["nats"] = "disconnected"
			healthy = false
		} else {
			details["nats"] = "ok"
		}
	}

	status := "healthy"
	code := http.StatusOK
	if !healthy {
		status = "unhealthy"
		code = http.StatusServiceUnavailable
	}

	HealthResponse{
		Status:  status,
		Details: details,
	}.Render(w, code)
}
