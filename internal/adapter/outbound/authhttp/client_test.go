package authhttp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func startH2CServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
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
			t.Errorf("decode request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.ActorID != "actor-1" {
			t.Errorf("actorId = %q", req.ActorID)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(evaluateResponse{Allowed: true, Reason: "granted"})
	}))

	client := New(srv.URL, nil, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.CheckPrivilege(ctx, "actor-1", "read", "policy-1")
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

	client := New(srv.URL, nil, zap.NewNop())
	result, err := client.CheckPrivilege(context.Background(), "actor-1", "read", "policy-1")
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

	client := New(srv.URL, nil, zap.NewNop())
	_, err := client.CheckPrivilege(context.Background(), "actor-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for degraded service")
	}
	if !strings.Contains(err.Error(), "degraded") {
		t.Errorf("error should mention degraded, got: %v", err)
	}
}

func TestH2CClient_BadRequest(t *testing.T) {
	srv := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid request"))
	}))

	client := New(srv.URL, nil, zap.NewNop())
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

	client := New(srv.URL, nil, zap.NewNop())
	_, err := client.CheckPrivilege(context.Background(), "actor-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestH2CClient_ConnectionRefused(t *testing.T) {
	// Open a listener on an ephemeral port, capture address, then close it.
	// This guarantees a recently-closed port that nothing is listening on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on ephemeral port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close ephemeral listener: %v", err)
	}

	client := New("http://"+addr, nil, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err = client.CheckPrivilege(ctx, "actor-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestH2CClient_NilClient(t *testing.T) {
	client := &Client{httpClient: nil, baseURL: "http://localhost", logger: zap.NewNop()}
	_, err := client.CheckPrivilege(context.Background(), "a", "b", "c")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}
