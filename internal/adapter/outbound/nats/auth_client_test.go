package nats

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
)

func TestAuthClient_Allowed(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(msg *natsgo.Msg) {
		resp := evaluateResponse{Allowed: true, Reason: "has read access"}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed=true")
	}
}

func TestAuthClient_Denied(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(msg *natsgo.Msg) {
		resp := evaluateResponse{Allowed: false, Reason: "no access"}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("expected allowed=false")
	}
}

func TestAuthClient_DegradedService(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(msg *natsgo.Msg) {
		resp := evaluateResponse{
			Allowed: false,
			Reason:  "circuit open",
			Error:   &errorDetails{Code: "circuit_breaker_open", Dependency: "database", RetryAfterMs: 5000},
		}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for degraded service")
	}
}

func TestAuthClient_Timeout(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(_ *natsgo.Msg) {
		// don't respond — simulate timeout
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestAuthClient_InvalidResponse(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte("not json"))
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestAuthClient_NonDegradedError(t *testing.T) {
	nc, cleanup := startTestNATS(t, func(msg *natsgo.Msg) {
		// Error with retryAfterMs=0 — not degraded, just denied
		resp := evaluateResponse{
			Allowed: false,
			Reason:  "policy denied",
			Error:   &errorDetails{Code: "denied", RetryAfterMs: 0},
		}
		data, _ := json.Marshal(resp)
		_ = msg.Respond(data)
	})
	defer cleanup()

	client := &AuthClient{Conn: nc, Subject: "auth.evaluate"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := client.CheckPrivilege(ctx, "agent-1", "read", "policy-1")
	if err != nil {
		t.Fatalf("non-degraded error should not return error: %v", err)
	}
	if result.Allowed {
		t.Error("expected allowed=false")
	}
}

// startTestNATS starts an in-process NATS server and returns a connected client.
func startTestNATS(t *testing.T, handler func(*natsgo.Msg)) (*natsgo.Conn, func()) {
	t.Helper()

	opts := &natsserver.Options{
		Host:           "127.0.0.1",
		Port:           -1, // random port
		NoLog:          true,
		NoSigs:         true,
		MaxControlLine: 4096,
	}

	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create test NATS server: %v", err)
	}
	srv.Start()

	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS server not ready")
	}

	nc, err := natsgo.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("failed to connect to test NATS: %v", err)
	}

	sub, err := nc.Subscribe("auth.evaluate", handler)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		t.Fatalf("subscribe: %v", err)
	}

	return nc, func() {
		_ = sub.Unsubscribe()
		nc.Close()
		srv.Shutdown()
	}
}
