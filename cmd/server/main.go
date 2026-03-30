package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/config"
)

func main() {
	logger, err := config.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	pool, err := connectDatabase(context.Background(), cfg.AlkemioDB)
	if err != nil {
		logger.Fatal("failed to connect to database", zap.Error(err))
	}
	defer pool.Close()
	logger.Info("database connected", zap.String("host", cfg.AlkemioDB.Host))

	nc, err := connectNATS(cfg.NATS, logger)
	if err != nil {
		logger.Fatal("failed to connect to NATS", zap.Error(err))
	}
	defer nc.Close()

	fileSvc := buildFileService(pool, nc, cfg)
	router := buildRouter(pool, nc, cfg, fileSvc, logger)
	srv := newHTTPServer(cfg.Port, router)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("server starting", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server failed", zap.Error(err))
		}
	}()

	<-done
	logger.Info("shutting down")
	shutdownServer(srv, logger)
	logger.Info("server stopped")
}
