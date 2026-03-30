package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	httpAdapter "github.com/alkem-io/file-service-go/internal/adapter/inbound/http"
	"github.com/alkem-io/file-service-go/internal/adapter/outbound/alkemiodb"
	natsAdapter "github.com/alkem-io/file-service-go/internal/adapter/outbound/nats"
	"github.com/alkem-io/file-service-go/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service-go/internal/config"
	"github.com/alkem-io/file-service-go/internal/domain/service"
	"github.com/alkem-io/file-service-go/internal/imaging"
	"github.com/alkem-io/file-service-go/internal/resilience"
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

	// Database pool
	pool, err := pgxpool.New(context.Background(), cfg.AlkemioDB.ConnString())
	if err != nil {
		logger.Fatal("failed to connect to database", zap.Error(err))
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		logger.Fatal("failed to ping database", zap.Error(err))
	}
	logger.Info("database connected", zap.String("host", cfg.AlkemioDB.Host))

	// NATS connection
	nc, err := resilience.ConnectNATS(cfg.NATS, logger)
	if err != nil {
		logger.Fatal("failed to connect to NATS", zap.Error(err))
	}
	defer nc.Close()

	// Adapters
	docRepo := alkemiodb.New(pool)
	authClient := &natsAdapter.AuthClient{Conn: nc, Subject: cfg.NATS.Subject}
	storageAdapter := local.New(cfg.StoragePath)
	imageProcessor := imaging.New()

	// Domain service
	fileSvc := &service.FileService{
		Repo:      docRepo,
		Auth:      authClient,
		Storage:   storageAdapter,
		Processor: imageProcessor,
	}

	// Initialize metrics
	httpAdapter.InitMetrics()

	// HTTP handlers
	maxAge := int(cfg.DocumentMaxAge.Seconds())
	publicHandler := &httpAdapter.PublicHandler{
		Repo:    docRepo,
		Auth:    authClient,
		Storage: storageAdapter,
		MaxAge:  maxAge,
	}
	documentHandler := &httpAdapter.DocumentHandler{
		Service: fileSvc,
		MaxAge:  maxAge,
	}
	healthHandler := &httpAdapter.HealthHandler{
		DB:   pool,
		NATS: nc,
	}

	// Router
	router := httpAdapter.NewRouter(httpAdapter.Deps{
		PublicHandler:   publicHandler,
		DocumentHandler: documentHandler,
		HealthHandler:   healthHandler,
		Logger:          logger,
	})

	// HTTP server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", zap.Error(err))
	}

	logger.Info("server stopped")
}
