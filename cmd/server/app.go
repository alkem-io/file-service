package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
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

func connectDatabase(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.ConnString())
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

func connectNATS(cfg config.NATSConfig, logger *zap.Logger) (*nats.Conn, error) {
	return resilience.ConnectNATS(cfg, logger)
}

func buildFileService(pool *pgxpool.Pool, nc *nats.Conn, cfg *config.Config) *service.FileService {
	return &service.FileService{
		Repo:      alkemiodb.New(pool),
		Auth:      &natsAdapter.AuthClient{Conn: nc, Subject: cfg.NATS.Subject},
		Storage:   local.New(cfg.StoragePath),
		Processor: imaging.New(),
	}
}

func buildRouter(pool *pgxpool.Pool, nc *nats.Conn, cfg *config.Config, fileSvc *service.FileService, logger *zap.Logger) http.Handler {
	httpAdapter.InitMetrics()

	maxAge := int(cfg.DocumentMaxAge.Seconds())

	return httpAdapter.NewRouter(httpAdapter.Deps{
		PublicHandler: &httpAdapter.PublicHandler{
			Repo:    fileSvc.Repo,
			Auth:    fileSvc.Auth,
			Storage: fileSvc.Storage,
			MaxAge:  maxAge,
			Logger:  logger,
		},
		DocumentHandler: &httpAdapter.DocumentHandler{
			Service: fileSvc,
			MaxAge:  maxAge,
			Logger:  logger,
		},
		HealthHandler: &httpAdapter.HealthHandler{
			DB:   pool,
			NATS: nc,
		},
		Logger: logger,
	})
}

func newHTTPServer(port int, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		Handler:        handler,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}
}

func shutdownServer(srv *http.Server, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", zap.Error(err))
	}
}
