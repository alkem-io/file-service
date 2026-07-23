// Package main boots the Alkemio file-service: it wires the hexagonal core
// (domain service + ports) to its adapters — pgx/sqlc repository, local
// filesystem storage, NATS or h2c auth transport, imaging processor — and
// serves the HTTP API until SIGINT/SIGTERM triggers a graceful shutdown.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// run dispatches the subcommand. The default (no args) is `serve` — the long-running HTTP server.
// Maintenance sweeps run as one-shot subcommands (invoked as k8s Jobs), sharing the same image and
// wiring but NOT the serving lifecycle — so a finite, converging one-off migration can't become a
// per-boot full-table scan on every serving pod.
func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return runServe()
	case "sweep-dims":
		return runSweepDims()
	default:
		fmt.Fprintf(os.Stderr, "file-service: unknown command %q (want: serve | sweep-dims)\n", cmd)
		return 2
	}
}

// runServe boots the long-running HTTP server and blocks until SIGINT/SIGTERM.
func runServe() int {
	base, cleanup, code := setupBase()
	if base == nil {
		return code
	}
	defer cleanup()
	logger, cfg, pool := base.logger, base.cfg, base.pool
	logger.Info("database connected", zap.String("host", cfg.AlkemioDB.Host))

	var err error
	// NATS is optional — only connect if transport is "nats"
	var nc *nats.Conn
	if cfg.AuthTransport == "nats" {
		nc, err = connectNATS(cfg.NATS, logger)
		if err != nil {
			logger.Error("failed to connect to NATS", zap.Error(err))
			return 1
		}
		defer nc.Close()
	}

	auth := buildAuthClient(cfg, nc, logger)
	logger.Info("auth transport configured", zap.String("transport", cfg.AuthTransport))

	fileSvc := buildFileService(pool, auth, cfg, logger)

	// Boot-time MIME repair (spec 019 FR-006): heal documents corrupted by
	// the pre-fix replace path. Idempotent; runs concurrently with serving.
	go runMimeRepair(context.Background(), fileSvc, logger)

	// The image-dimension backfill (spec 019/020) is NOT run here — it is a one-shot `sweep-dims`
	// subcommand (a k8s Job), so a converging legacy migration isn't a per-boot full-table scan on
	// every serving pod. New writes measure dims in the write pipeline; a dedup re-upload heals its
	// row on the spot.

	// Periodic backup-outbox prune (008 SC-008), only when the producer is on.
	// Cancelled on shutdown so the goroutine exits cleanly.
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()
	if cfg.BackupOutbox.Enabled {
		go runOutboxPrune(bgCtx, fileSvc, cfg.BackupOutbox.DoneRetention, logger)
	}

	router := buildRouter(pool, nc, cfg, fileSvc, logger)
	srv := newHTTPServer(cfg.Port, router)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	exitCode := 0
	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	case err := <-errCh:
		logger.Error("server failed", zap.Error(err))
		exitCode = 1
	}

	shutdownServer(srv, logger)
	logger.Info("server stopped")
	return exitCode
}

func main() {
	os.Exit(run(os.Args[1:]))
}
