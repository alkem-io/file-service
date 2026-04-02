package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/config"
)

func TestConnectDatabase_Success(t *testing.T) {
	cfg := config.DatabaseConfig{
		Host:     "localhost",
		Port:     5432,
		Username: "synapse",
		Password: "synapse", //nolint:gosec // test credentials
		Name:     "alkemio",
	}

	pool, err := connectDatabase(context.Background(), cfg)
	if err != nil {
		t.Skipf("DB not available: %v", err)
	}
	defer pool.Close()

	if pool == nil {
		t.Fatal("nil pool")
	}
}

func TestConnectDatabase_BadHost(t *testing.T) {
	cfg := config.DatabaseConfig{
		Host:     "127.0.0.1",
		Port:     1, // nothing here
		Username: "x",
		Password: "x",
		Name:     "x",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := connectDatabase(ctx, cfg)
	if err == nil {
		t.Fatal("expected error for bad host")
	}
}

func TestConnectNATS_Success(t *testing.T) {
	srv := startTestNATSServer(t)
	defer srv.Shutdown()

	logger, _ := zap.NewDevelopment()
	cfg := config.NATSConfig{
		URL:                 srv.ClientURL(),
		ReconnectWaitMS:     100,
		ReconnectMaxWaitMS:  1000,
		FailureThreshold:    3,
		BreakerTimeoutSecs:  5,
		HalfOpenMaxRequests: 2,
	}

	nc, err := connectNATS(cfg, logger)
	if err != nil {
		t.Fatalf("connectNATS: %v", err)
	}
	defer nc.Close()

	if !nc.IsConnected() {
		t.Error("not connected")
	}
}

func TestConnectNATS_BadURL(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cfg := config.NATSConfig{
		URL:                 "nats://127.0.0.1:1",
		ReconnectWaitMS:     100,
		ReconnectMaxWaitMS:  200,
		FailureThreshold:    1,
		BreakerTimeoutSecs:  5,
		HalfOpenMaxRequests: 1,
	}

	_, err := connectNATS(cfg, logger)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBuildFileService(t *testing.T) {
	// Just verify it doesn't panic with nil-safe paths
	// We can't pass real connections but can verify the function signature works
	cfg := &config.Config{
		StoragePath: t.TempDir(),
		NATS:        config.NATSConfig{Subject: "auth.evaluate"},
	}

	// Use a real pool just to construct (won't be called)
	pool, _ := pgxpool.New(context.Background(), "postgres://invalid:5432/x")
	if pool == nil {
		// pgxpool.New may return nil pool with error for invalid config
		t.Skip("cannot create pool stub")
	}

	logger, _ := zap.NewDevelopment()
	svc := buildFileService(pool, nil, cfg, logger)
	if svc == nil {
		t.Fatal("nil service")
	}
	if svc.Repo == nil {
		t.Error("nil repo")
	}
	if svc.Storage == nil {
		t.Error("nil storage")
	}
	if svc.Processor == nil {
		t.Error("nil processor")
	}
}

func TestNewHTTPServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := newHTTPServer(0, handler)
	if srv == nil {
		t.Fatal("nil server")
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 60*time.Second {
		t.Errorf("WriteTimeout = %v", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v", srv.IdleTimeout)
	}
}

func TestShutdownServer(_ *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(handler)
	logger, _ := zap.NewDevelopment()

	// Shutdown should not panic
	shutdownServer(srv.Config, logger)
}

func startTestNATSServer(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS not ready")
	}
	return srv
}
