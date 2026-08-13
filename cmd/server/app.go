package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
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

// appBase is the wiring every subcommand needs: a logger, the loaded config, and a DB pool.
type appBase struct {
	logger *zap.Logger
	cfg    *config.Config
	pool   *pgxpool.Pool
}

// setupBase builds the shared bootstrap (logger → config → DB pool) and returns it with a cleanup
// func to defer. On any failure it reports and returns (nil, nil, nonzero-code); callers return
// that code. Shared by `serve` and every sweep subcommand so they can't drift on bootstrap.
func setupBase() (*appBase, func(), int) {
	return setupBaseWith(config.Load)
}

func setupSweepBase() (*appBase, func(), int) {
	return setupBaseWith(config.LoadForSweep)
}

func setupBaseWith(loadConfig func() (*config.Config, error)) (*appBase, func(), int) {
	logger, err := config.NewLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		return nil, nil, 1
	}
	cfg, err := loadConfig()
	if err != nil {
		logger.Error("failed to load config", zap.Error(err))
		_ = logger.Sync()
		return nil, nil, 1
	}
	pool, err := connectDatabase(context.Background(), cfg.AlkemioDB)
	if err != nil {
		logger.Error("failed to connect to database", zap.Error(err))
		_ = logger.Sync()
		return nil, nil, 1
	}
	cleanup := func() {
		pool.Close()
		_ = logger.Sync()
	}
	return &appBase{logger: logger, cfg: cfg, pool: pool}, cleanup, 0
}

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
		// Spec 020 (FR-009): ordinary request bodies retain the fixed 30 s
		// read cap; upload handlers override it with a rolling deadline on
		// every read (progressReader). WriteTimeout must be zero because it is
		// an absolute deadline starting after request headers, so it would
		// kill healthy long uploads/downloads based on duration alone. Blob
		// and ordinary responses use a lazy rolling write-idle deadline in
		// the HTTP router instead.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
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

// runSweepDims runs the image-dimension backfill ONCE and exits — the one-shot `sweep-dims`
// subcommand, invoked as a k8s Job after a deploy to drain the finite legacy set (spec 019/020).
// At runtime the sweep never authorizes (it only reads storage + writes content_metadata), so it
// builds the service with a nil auth port and no NATS. LoadForSweep validates the shared storage,
// database, imaging, and ingest config without requiring an unused auth transport. SIGINT/SIGTERM
// cancels the sweep for a clean Job termination; exit code is dimsJobExitCode.
func runSweepDims() int {
	base, cleanup, code := setupSweepBase()
	if base == nil {
		return code
	}
	defer cleanup()
	logger := base.logger

	fileSvc := buildFileService(base.pool, nil, base.cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("dims-backfill job starting")
	sum := fileSvc.RunDimsBackfill(ctx)
	if sum.Aborted {
		logger.Warn("dims-backfill job ENDED EARLY — rerun to finish",
			zap.Int("measured", sum.Measured),
			zap.Int("decode_failed", sum.DecodeFailed),
			zap.Int("skipped", sum.Skipped))
	} else {
		logger.Info("dims-backfill job complete",
			zap.Int("measured", sum.Measured),
			zap.Int("decode_failed", sum.DecodeFailed),
			zap.Int("skipped", sum.Skipped))
	}
	return dimsJobExitCode(sum)
}

// dimsJobExitCode maps a sweep outcome to a k8s Job exit code — extracted so the retry policy is
// unit-testable (mirrors file-backup-service's backfillVerdict, and the same reasoning). Exit 1 ONLY
// when the pass ENDED EARLY (Aborted: a page-scan error or a shutdown signal) — the one hard failure
// where the scan itself broke, so a rerun is warranted. Everything else is exit 0.
//
// Skips do NOT gate the exit code, because an all-skipped pass is genuinely AMBIGUOUS and the exit
// code cannot tell the two cases apart: it can be the sweep's convergent tail (a handful of rows
// whose blobs are permanently gone / reliably panic libvips — they will skip on EVERY future run,
// so failing on them means the Job can never reach a clean exit 0 and an operator's rerun loop never
// terminates), OR a transient storage/DB outage (skips everything, but self-heals next run). The
// operator running the Job reads the logged measured/decode_failed/skipped summary to tell them
// apart — a heuristic exit code would be wrong in one direction or the other. (This is exactly the
// all-skipped ruling from file-backup-service's backfillVerdict.)
func dimsJobExitCode(sum service.DimsBackfillSummary) int {
	if sum.Aborted {
		return 1
	}
	return 0
}

// runSweepCIDs runs the legacy-name normalization ONCE and exits — the one-shot `sweep-cids`
// subcommand (018-legacy-cid-normalization), invoked as a manually-triggered k8s Job. Like
// `sweep-dims` it never authorizes, so it builds the service with a nil auth port and no NATS.
//
// This migration is IRREVERSIBLE (the legacy blob is reclaimed in the same pass), which is why
// --dry-run exists and why the run report is written to the mounted store rather than only logged.
// SIGINT/SIGTERM cancels the pass for a clean Job termination; the pass is resumable, so an
// interrupted run is simply re-run.
func runSweepCIDs(args []string) int {
	fs := flag.NewFlagSet("sweep-cids", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false,
		"enumerate and report the affected count, changing nothing at all")
	// A string rather than a float so "unset" is representable. The empty default
	// distinguishes "no --rate given" from "--rate 0" without inspecting which
	// flags the parser visited, and it lets a malformed value say so precisely.
	rate := fs.String("rate", "",
		"objects/second ceiling, a positive finite number (default: the conservative built-in rate)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // asking for usage is not a failure
		}
		return 2 // invalid flags — same class as an unknown subcommand
	}
	// Go's flag parser STOPS at the first non-flag operand, so `sweep-cids prod --dry-run`
	// parses cleanly with dry-run FALSE and runs the irreversible pass. A stray word in a Job's
	// args:, or one `--` too many in `kubectl create job ... -- sweep-cids -- --dry-run`, would
	// silently turn a requested preview into a full corpus rewrite. Nothing here takes a
	// positional argument, so any is a mistake.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr,
			"file-service: sweep-cids takes no positional arguments, got %q — flags after one are IGNORED by the parser, which would silently drop --dry-run\n",
			fs.Args())
		return 2
	}

	// Rejected rather than read as "unlimited": each object costs a blob read, a blob write and a
	// DB round-trip against shared production infrastructure, so an unbounded pass is a
	// self-inflicted load test (FR-015). ParseFloat accepts "NaN" and "Inf"; either would stop
	// the limiter bounding anything AND make the run report unencodable, so both are refused —
	// !(v > 0) rather than v <= 0, because every comparison against NaN is false.
	parsedRate, err := parseSweepRate(*rate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "file-service: %v\n", err)
		return 2
	}

	base, cleanup, code := setupSweepBase()
	if base == nil {
		return code
	}
	defer cleanup()
	logger := base.logger

	// An unmounted PVC, or LOCAL_STORAGE_PATH left at its relative dev default, makes EVERY read
	// return ENOENT — which the sweep correctly classifies as "content permanently absent". The
	// pass would then report zero failures, exit 0, and brand the whole corpus unrecoverable in
	// the one durable record of the migration. The storage adapter deliberately does not tell an
	// absent blob from an absent store ("that judgement belongs to the caller that knows what
	// SHOULD exist"); this is that caller making it, once, before touching anything.
	fileSvc := buildSweepCIDsService(base.pool, base.cfg, logger)
	if err := verifySweepStorage(fileSvc.Storage, base.cfg.StoragePath); err != nil {
		logger.Error("sweep-cids: refusing to run — the content store is not usable",
			zap.String("path", base.cfg.StoragePath), zap.Error(err))
		return 1
	}

	effectiveRate := base.cfg.SweepCIDs.DefaultRate
	if parsedRate != nil {
		effectiveRate = *parsedRate
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sink := local.NewReportSink(base.cfg.StoragePath, base.cfg.SweepCIDs.ReportDir).
		WithLogf(func(format string, args ...any) { logger.Warn(fmt.Sprintf(format, args...)) })

	logger.Info("sweep-cids job starting",
		zap.Bool("dry_run", *dryRun),
		zap.Float64("rate", effectiveRate),
		zap.String("reports", sink.Dir()))
	sum := fileSvc.RunCIDNormalize(ctx, service.CIDNormalizeOptions{
		DryRun: *dryRun,
		Rate:   effectiveRate,
		// The service, not this caller, enforces that a dry run writes nothing —
		// keeping the FR-007 rule in one place instead of two.
		Report: sink,
	})
	return cidsJobExitCode(sum)
}

// parseSweepRate turns the --rate flag into an override, or nil when it was not given.
func parseSweepRate(raw string) (*float64, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, fmt.Errorf("--rate %q is not a number", raw)
	}
	if !(v > 0) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("--rate must be a positive, finite number (got %v); it bounds load on shared production infrastructure and has no 'unlimited' setting", v)
	}
	return &v, nil
}

// buildSweepCIDsService wires only what the legacy-name sweep touches: the repository
// and the blob store.
//
// The backup outbox is deliberately NOT wired. An earlier version did, with a comment
// claiming reclamation dropped pending hints "exactly as every other deletion does" —
// it did not: the sweep never calls DeletePendingByHash, and since it PARKS rather than
// deletes, there is nothing to clean up after. A pending hint naming a parked legacy
// name resolves to nothing and is handled by the consumer's existing 404-skip backstop.
// Dead wiring plus a comment asserting behaviour that does not exist is worse than
// neither, because an operator sets production config on the strength of it.
//
// It deliberately does NOT go through buildFileService, whose first act is to start
// libvips and configure its streaming knobs. This sweep decodes nothing: it copies bytes
// and renames rows. Booting an image library in a one-shot Job that will never call it
// costs startup time and memory in a process whose whole job is to be gentle on shared
// production infrastructure, and it makes the Job fail for reasons unrelated to its work.
func buildSweepCIDsService(pool *pgxpool.Pool, cfg *config.Config, logger *zap.Logger) *service.FileService {
	repo := alkemiodb.NewWithLogger(pool, logger)
	svc := &service.FileService{
		Repo:    repo,
		Storage: local.New(cfg.StoragePath),
		Logger:  logger,
	}
	return svc
}

// verifySweepStorage checks the content store is actually there before an irreversible pass reads
// "absent" as a fact about the data. A non-empty directory is the bar: an empty one is either an
// unmounted volume or a store with nothing to sweep, and the sweep has no business guessing which.
// verifySweepStorage asks the STORE whether it is usable and populated, rather than
// walking the directory here. An unmounted volume makes every read look like "this
// content is permanently gone", so the pass would touch nothing, exit 0, and write a
// report branding the whole corpus unrecoverable.
//
// This used to re-implement the adapter's chunked walk and blob classification in
// cmd/. Two copies of "what counts as content" is the duplication CLAUDE.md forbids,
// and it could drift into a pre-flight that passes on entries the scan ignores — it
// also hardcoded local-filesystem semantics into a check that must outlive the local
// backend.
func verifySweepStorage(store port.StoragePort, path string) error {
	populated, err := store.HasContent()
	if err != nil {
		return err
	}
	if !populated {
		return fmt.Errorf("%s holds no content — an unmounted volume looks exactly like a corpus whose blobs are all gone, and this sweep cannot tell them apart from the inside", path)
	}
	return nil
}

func cidsJobExitCode(sum service.CIDNormalizeSummary) int {
	// Truncated gates the exit code too: the pass stopped short of the corpus, so
	// exiting 0 would tell the Job controller — and the operator — that there is
	// nothing left to do.
	if sum.Aborted || sum.Failed > 0 || sum.ReportFailed || sum.Truncated {
		return 1
	}
	return 0
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
