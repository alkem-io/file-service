package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	gobreaker "github.com/sony/gobreaker/v2"

	httpAdapter "github.com/alkem-io/file-service/internal/adapter/inbound/http"
	"github.com/alkem-io/file-service/internal/adapter/outbound/alkemiodb"
	"github.com/alkem-io/file-service/internal/adapter/outbound/authhttp"
	natsAdapter "github.com/alkem-io/file-service/internal/adapter/outbound/nats"
	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/config"
	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
	"github.com/alkem-io/file-service/internal/imaging"
	"github.com/alkem-io/file-service/internal/resilience"
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

func buildAuthClient(cfg *config.Config, nc *nats.Conn, logger *zap.Logger) port.AuthPort {
	switch cfg.AuthTransport {
	case "h2c":
		cb := gobreaker.NewCircuitBreaker[model.AuthResult](gobreaker.Settings{
			Name:        "auth-h2c",
			MaxRequests: uint32(cfg.Breaker.HalfOpenMaxRequests), //nolint:gosec // validated positive in config.Load
			Timeout:     time.Duration(cfg.Breaker.TimeoutSecs) * time.Second,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return int(counts.ConsecutiveFailures) >= cfg.Breaker.FailureThreshold
			},
			OnStateChange: func(name string, from, to gobreaker.State) {
				logger.Info("circuit breaker state change",
					zap.String("name", name),
					zap.String("from", from.String()),
					zap.String("to", to.String()))
			},
		})
		return authhttp.New(cfg.AuthServiceURL, cb, logger.Named("auth-h2c"))
	default:
		return &natsAdapter.AuthClient{Conn: nc, Subject: cfg.NATS.Subject}
	}
}

func buildFileService(pool *pgxpool.Pool, auth port.AuthPort, cfg *config.Config, logger *zap.Logger) *service.FileService {
	return &service.FileService{
		Repo:      alkemiodb.New(pool),
		Auth:      auth,
		Storage:   local.New(cfg.StoragePath),
		Processor: imaging.New(),
		Logger:    logger,
	}
}

func buildRouter(pool *pgxpool.Pool, nc *nats.Conn, cfg *config.Config, fileSvc *service.FileService, logger *zap.Logger) http.Handler { //nolint:unparam // nc can be nil when using h2c
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
		HealthHandler: newHealthHandler(pool, nc),
		Logger:        logger,
	})
}

func newHealthHandler(pool *pgxpool.Pool, nc *nats.Conn) *httpAdapter.HealthHandler {
	h := &httpAdapter.HealthHandler{DB: pool}
	if nc != nil {
		h.NATS = nc // only set interface when concrete value is non-nil (avoids Go nil interface trap)
	}
	return h
}

func newHTTPServer(port int, handler http.Handler) *http.Server {
	h2s := &http2.Server{
		MaxConcurrentStreams: 100,
		IdleTimeout:          120 * time.Second,
	}
	return &http.Server{
		Addr:           fmt.Sprintf(":%d", port),
		Handler:        h2c.NewHandler(handler, h2s),
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

// runMimeRepair executes the boot-time MIME repair (spec 019 FR-006) and
// feeds the summary into the mime_repair_total metric. Failures are logged,
// never fatal — the job is independent of request serving and reruns on the
// next boot.
func runMimeRepair(ctx context.Context, fileSvc *service.FileService, logger *zap.Logger) {
	httpAdapter.InitMetrics()
	sum := fileSvc.RunMimeRepair(ctx)
	httpAdapter.MimeRepairOps.Add("relabeled", int64(sum.Relabeled))
	httpAdapter.MimeRepairOps.Add("unrecoverable", int64(sum.Unrecoverable))
	httpAdapter.MimeRepairOps.Add("skipped_not_office", int64(sum.SkippedNotOffice))
	httpAdapter.MimeRepairOps.Add("errors", int64(sum.Errors))
	logger.Info("mime-repair metrics recorded",
		zap.Int("relabeled", sum.Relabeled),
		zap.Int("unrecoverable", sum.Unrecoverable),
		zap.Int("skipped_not_office", sum.SkippedNotOffice),
		zap.Int("errors", sum.Errors))
}
