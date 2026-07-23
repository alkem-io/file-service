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

	"github.com/alkem-io/file-service/internal/config"
	"github.com/alkem-io/file-service/internal/domain/service"
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
		URL:                srv.ClientURL(),
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 1000,
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
		URL:                "nats://127.0.0.1:1",
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 200,
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
		t.Skip("cannot create pool stub")
	}
	defer pool.Close()

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
	// Ordinary bodies keep the fixed cap; upload handlers replace it with
	// a rolling per-read deadline.
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	// A global WriteTimeout is absolute from request headers and would
	// kill healthy long transfers. The router applies a lazy rolling idle cap.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0", srv.WriteTimeout)
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

// TestRun_UnknownCommand: a typo'd subcommand must fail loudly (exit 2), never fall through to
// serve or silently succeed.
func TestRun_UnknownCommand(t *testing.T) {
	if code := run([]string{"bogus-cmd"}); code != 2 {
		t.Fatalf("run(unknown) = %d, want 2", code)
	}
}

// TestDimsJobExitCode: the k8s Job retry contract — exit 1 ONLY on an ENDED-EARLY (Aborted) pass.
// Skips never gate the exit code (an all-skipped pass is ambiguous — a permanent orphan tail vs a
// transient outage — and a heuristic would either loop forever on the tail or miss a mid-run outage;
// the operator reads the logged counts). Matches file-backup-service's backfillVerdict all-skipped ruling.
func TestDimsJobExitCode(t *testing.T) {
	cases := []struct {
		name string
		sum  service.DimsBackfillSummary
		want int
	}{
		{"empty-drained-corpus", service.DimsBackfillSummary{}, 0},
		{"progress-with-some-skips", service.DimsBackfillSummary{Measured: 5, Skipped: 3, DecodeFailed: 1}, 0},
		{"all-skipped-ambiguous-not-a-failure", service.DimsBackfillSummary{Skipped: 100}, 0},
		{"convergent-orphan-tail", service.DimsBackfillSummary{Skipped: 5}, 0},
		{"aborted-after-progress", service.DimsBackfillSummary{Measured: 2, Aborted: true}, 1},
		// The load-bearing case: a total outage that fails the FIRST page scan yields Aborted with
		// zero of everything — Aborted alone (NOT coupled with progress) must gate exit 1, or a
		// mid/total outage on page one would silently exit 0.
		{"aborted-total-first-page-outage", service.DimsBackfillSummary{Aborted: true}, 1},
	}
	for _, c := range cases {
		if got := dimsJobExitCode(c.sum); got != c.want {
			t.Errorf("%s: exit = %d, want %d", c.name, got, c.want)
		}
	}
}
