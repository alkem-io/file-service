package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	gobreaker "github.com/sony/gobreaker/v2"
	"go.uber.org/zap"

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
	// imaging.New runs vips.Startup (vips build); the streaming knobs must
	// be applied to an initialized libvips, so configure AFTER New.
	processor := imaging.New()
	imaging.ConfigureStreaming(imaging.StreamingConfig{
		DiscThreshold: cfg.Ingest.VipsDiscThreshold,
		ScratchDir:    cfg.Ingest.VipsScratchDir,
		PipeReadLimit: cfg.Ingest.VipsPipeReadLimit,
		PixelBudget:   cfg.Ingest.PixelBudget,
	})
	repo := alkemiodb.NewWithLogger(pool, logger)
	svc := &service.FileService{
		Repo:            repo,
		Auth:            auth,
		Storage:         local.New(cfg.StoragePath),
		Processor:       processor,
		Logger:          logger,
		HotMimePrefixes: cfg.BackupOutbox.HotMimePrefixes,
	}
	// 008-continuous-file-backup: when the producer is enabled, the SAME adapter drives the
	// transactional outbox writes (it implements both DocumentRepo and BackupOutboxRepo). Off =
	// nil Outbox = the create/replace paths use the original non-transactional Repo methods.
	if cfg.BackupOutbox.Enabled {
		svc.Outbox = repo
		logger.Info("continuous-backup outbox producer enabled",
			zap.Int("hotMimePrefixes", len(cfg.BackupOutbox.HotMimePrefixes)))
	}
	return svc
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
			Service:       fileSvc,
			MaxAge:        maxAge,
			Logger:        logger,
			MaxUploadSize: cfg.Ingest.MaxUploadSize,
			IdleTimeout:   cfg.Ingest.IdleTimeout,
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
	// Native unencrypted HTTP/2 (Go ≥1.24) replaces the deprecated
	// x/net h2c wrapper; in-cluster clients (server adapter, wopi) speak
	// h2c to this service.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Addr:      fmt.Sprintf(":%d", port),
		Handler:   handler,
		Protocols: protocols,
		HTTP2:     &http.HTTP2Config{MaxConcurrentStreams: 100},
		// Spec 020 (FR-009): the global ReadTimeout is replaced by a header
		// deadline + per-request read deadlines. Upload handlers extend the
		// deadline on progress (progressReader); every other route gets a
		// fixed 30 s read deadline from the router middleware, preserving
		// the pre-020 behavior.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
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

// runDimsBackfill executes the boot-time image-dimension backfill (spec 019/020) and feeds the
// summary into the dims_backfill_total metric. Like the MIME repair it is independent of request
// serving, logged-never-fatal, and reruns each boot — converging to a near-empty scan once the
// legacy set is drained.
func runDimsBackfill(ctx context.Context, fileSvc *service.FileService, logger *zap.Logger) {
	httpAdapter.InitMetrics()
	sum := fileSvc.RunDimsBackfill(ctx)
	httpAdapter.DimsBackfillOps.Add("measured", int64(sum.Measured))
	httpAdapter.DimsBackfillOps.Add("decode_failed", int64(sum.DecodeFailed))
	httpAdapter.DimsBackfillOps.Add("skipped", int64(sum.Skipped))
	logger.Info("dims-backfill metrics recorded",
		zap.Int("measured", sum.Measured),
		zap.Int("decode_failed", sum.DecodeFailed),
		zap.Int("skipped", sum.Skipped))
}

// runOutboxPrune periodically drops consumer-finished backup-outbox rows older than the
// retention window (008-continuous-file-backup SC-008), until ctx is cancelled. Best-effort —
// a prune failure is logged, never fatal. Only launched when the producer is enabled.
func runOutboxPrune(ctx context.Context, fileSvc *service.FileService, retention time.Duration, logger *zap.Logger) {
	const interval = time.Hour
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := fileSvc.PruneBackupOutbox(ctx, retention)
			if err != nil {
				logger.Warn("backup-outbox prune failed", zap.Error(err))
				continue
			}
			if n > 0 {
				logger.Info("backup-outbox pruned", zap.Int64("rows", n))
			}
		}
	}
}
