package authhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func startH2CServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	h2s := &http2.Server{}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(handler, h2s))
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestH2CClient_Allowed(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/internal/auth/evaluate" {
			t.Errorf("path = %s", r.URL.Path)
		}

		var req evaluateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.AgentID != "agent-1" {
			t.Errorf("agentId = %q", req.AgentID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(evaluateResponse{Allowed: true, Reason: "granted"})
	}))

	client := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err != nil {
		t.Fatalf("CheckPrivilege: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed=true")
	}
	if result.Reason != "granted" {
		t.Errorf("reason = %q", result.Reason)
	}
}

func TestH2CClient_Denied(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(evaluateResponse{Allowed: false, Reason: "no access"})
	}))

	client := New(srv.URL)
	result, err := client.CheckPrivilege(context.Background(), "agent-1", "read", "policy-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed {
		t.Error("expected allowed=false")
	}
}

func TestH2CClient_ServiceDegraded(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(evaluateResponse{
			Allowed: false,
			Reason:  "circuit open",
			Error:   &errorDetails{Code: "circuit_breaker_open", Dependency: "database", RetryAfterMs: 5000},
		})
	}))

	client := New(srv.URL)
	_, err := client.CheckPrivilege(context.Background(), "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for degraded service")
	}
}

func TestH2CClient_BadRequest(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid request"))
	}))

	client := New(srv.URL)
	_, err := client.CheckPrivilege(context.Background(), "", "", "")
	if err == nil {
		t.Fatal("expected error for bad request")
	}
}

func TestH2CClient_InvalidJSON(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))

	client := New(srv.URL)
	_, err := client.CheckPrivilege(context.Background(), "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestH2CClient_ConnectionRefused(t *testing.T) {
	client := New("http://127.0.0.1:1") // nothing listening
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestH2CClient_NilClient(t *testing.T) {
	client := &Client{httpClient: nil, baseURL: "http://localhost"}
	_, err := client.CheckPrivilege(context.Background(), "a", "b", "c")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}
