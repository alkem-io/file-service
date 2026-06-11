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

	"github.com/alkem-io/file-service-go/internal/config"
)

func run() int {
	logger, err := config.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		return 1
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", zap.Error(err))
		return 1
	}

	pool, err := connectDatabase(context.Background(), cfg.AlkemioDB)
	if err != nil {
		logger.Error("failed to connect to database", zap.Error(err))
		return 1
	}
	defer pool.Close()
	logger.Info("database connected", zap.String("host", cfg.AlkemioDB.Host))

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
	os.Exit(run())
}
