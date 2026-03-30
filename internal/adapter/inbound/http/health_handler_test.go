package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type mockPinger struct {
	err error
}

func (m *mockPinger) Ping(_ context.Context) error { return m.err }

type mockConnChecker struct {
	connected bool
}

func (m *mockConnChecker) IsConnected() bool { return m.connected }

func TestHealth_AllHealthy(t *testing.T) {
	h := &HealthHandler{
		DB:   &mockPinger{},
		NATS: &mockConnChecker{connected: true},
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["status"] != "healthy" {
		t.Errorf("status = %v", body["status"])
	}
	details := body["details"].(map[string]any)
	if details["database"] != "ok" {
		t.Errorf("database = %v", details["database"])
	}
	if details["nats"] != "ok" {
		t.Errorf("nats = %v", details["nats"])
	}
}

func TestHealth_DBDown(t *testing.T) {
	h := &HealthHandler{
		DB:   &mockPinger{err: errors.New("connection refused")},
		NATS: &mockConnChecker{connected: true},
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}

	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["status"] != "unhealthy" {
		t.Errorf("status = %v", body["status"])
	}
	details := body["details"].(map[string]any)
	if details["database"] != "unreachable" {
		t.Errorf("database = %v", details["database"])
	}
	if details["nats"] != "ok" {
		t.Errorf("nats = %v", details["nats"])
	}
}

func TestHealth_NATSDown(t *testing.T) {
	h := &HealthHandler{
		DB:   &mockPinger{},
		NATS: &mockConnChecker{connected: false},
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}

	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	details := body["details"].(map[string]any)
	if details["nats"] != "disconnected" {
		t.Errorf("nats = %v", details["nats"])
	}
}

func TestHealth_BothDown(t *testing.T) {
	h := &HealthHandler{
		DB:   &mockPinger{err: errors.New("down")},
		NATS: &mockConnChecker{connected: false},
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}

	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["status"] != "unhealthy" {
		t.Errorf("status = %v", body["status"])
	}
}

func TestHealth_NATSNil(t *testing.T) {
	h := &HealthHandler{
		DB:   &mockPinger{},
		NATS: nil,
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}
