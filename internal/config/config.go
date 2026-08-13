// Package config loads and validates the service configuration from
// environment variables — HTTP port, storage location, database and NATS
// settings, circuit-breaker tuning, and the spec-020 streaming-ingest knobs —
// and constructs the production zap logger. Invalid or missing required
// values fail fast at startup with the offending variable named.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration of the service, assembled
// from environment variables by Load.
type Config struct {
	Port           int
	StoragePath    string
	StorageType    string
	DocumentMaxAge time.Duration
	AlkemioDB      DatabaseConfig
	NATS           NATSConfig
	Breaker        BreakerConfig
	AuthServiceURL string // h2c URL for auth-evaluation-service (e.g., "http://auth-service:6060")
	AuthTransport  string // "nats" or "h2c" — auto-detected from env vars
	Ingest         IngestConfig
	BackupOutbox   BackupOutboxConfig
	SweepCIDs      SweepCIDsConfig
}

// SweepCIDsConfig governs the one-shot legacy-name normalization sweep
// (018-legacy-cid-normalization). It is read only by the `sweep-cids`
// subcommand; the serving path never consults it.
type SweepCIDsConfig struct {
	// DefaultRate bounds objects/second when the operator does not pass --rate.
	// FR-015 requires a default low enough that a full pass satisfies SC-008 (no
	// user-visible degradation, no maintenance window) — that criterion, not
	// taste, is what "conservative" means here. 5/s matches the backup backfill
	// job's starting rate, so operators meet one number rather than two.
	DefaultRate float64
	// ReportDir is the directory, relative to StoragePath, that run reports are
	// written to (FR-018).
	//
	// The name is collision-proof BY CONSTRUCTION, not by convention: the store's
	// key rule (local.IsBlobName) accepts only 32-128 alphanumeric characters,
	// so "_sweep-reports" is disqualified twice over — under the length minimum,
	// and '_'/'-' are outside the alphabet. No blob can ever be named this, so no
	// enumeration of the store can mistake a report for content.
	ReportDir string
}

// BackupOutboxConfig governs the continuous-backup producer (008-continuous-file-backup):
// when Enabled, every committed non-temporary document also commits a `file_backup_outbox`
// row in the SAME transaction (FR-001) and emits NOTIFY. Off by default — a pure opt-in.
type BackupOutboxConfig struct {
	// Enabled turns the transactional outbox producer on. Default false.
	Enabled bool
	// HotMimePrefixes are MIME-type prefixes whose objects get outbox priority=1 (hot) so
	// their effective RPO is lowest (documents/office/Yjs — the RPO driver). Everything else
	// is priority=0. Configurable so the hot set lives in file-service config (research R2).
	HotMimePrefixes []string
	// DoneRetention is how long a consumer-finished (status='done') outbox row is kept before
	// the periodic prune drops it — keeps the shared outbox bounded (SC-008). The durable
	// record lives in the backup ledger; done rows are just a debugging window.
	DoneRetention time.Duration
}

const (
	// defaultSweepCIDsRate is the objects/second ceiling applied when the operator
	// does not pass --rate. Deliberately not an env var: the sweep is a one-shot
	// operator-triggered migration, so the knob belongs on the command line where
	// the person running it can see and reason about it, not in ambient config.
	defaultSweepCIDsRate = 5.0
	// sweepCIDsReportDir is the reserved directory (under StoragePath) for run
	// reports. The name is owned by the local storage adapter, which owns the key
	// rule that makes it collision-proof; config cannot import that package
	// (adapters depend on config, not the reverse), so the two are kept in step by
	// TestSweepCIDsReportDirMatchesTheStorageReservation rather than by hope.
	sweepCIDsReportDir = "_sweep-reports"
)

// defaultHotMimePrefixes marks the user-authored, non-reconstructable document classes hot.
var defaultHotMimePrefixes = []string{
	"application/vnd.openxmlformats-officedocument", // docx / xlsx / pptx
	"application/vnd.oasis.opendocument",            // odt / ods / odp
	"application/msword",                            // legacy .doc
	"application/vnd.ms-",                           // legacy .xls / .ppt
	"application/x-yjs",                             // Yjs blobs (whiteboards, memos)
}

// IngestConfig governs the streaming upload pipeline (spec 020).
type IngestConfig struct {
	// MaxUploadSize is the hard incremental cap on any single upload body,
	// in bytes. Default keeps today's 32 MiB behavior; validated to 1 GiB.
	MaxUploadSize int64
	// IdleTimeout aborts an upload after this long without receiving bytes
	// (progress-based — a slow-but-moving upload is never killed).
	IdleTimeout time.Duration
	// PixelBudget rejects images whose header-declared width×height exceeds
	// it, before any pixel decode (FR-010).
	PixelBudget int64
	// VipsDiscThreshold (bytes) spills decoded frames above it to scratch
	// disc instead of RAM. 0 = library default (VIPS_DISC_THRESHOLD/100MB).
	VipsDiscThreshold int64
	// VipsScratchDir hosts materialized frames. Empty = os.TempDir().
	VipsScratchDir string
	// VipsPipeReadLimit (bytes) bounds compressed-input accumulation for
	// non-seekable seek-needing loads. 0 = library default.
	VipsPipeReadLimit int64
}

// BreakerConfig is shared circuit breaker settings for any auth transport.
type BreakerConfig struct {
	FailureThreshold    int
	TimeoutSecs         int
	HalfOpenMaxRequests int
}

// DatabaseConfig holds the Postgres connection settings for Alkemio's
// shared database. All fields are required; Load fails fast when any
// ALKEMIO_DATABASE_* variable is missing.
type DatabaseConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	Name     string
}

// ConnString renders the pgx connection URL. Credentials are URL-escaped,
// so passwords may contain reserved characters; sslmode=disable is fixed
// because the database is only reachable inside the cluster network.
func (d DatabaseConfig) ConnString() string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(d.Username, d.Password),
		Host:     fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:     d.Name,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// NATSConfig holds the NATS connection and reconnect-backoff settings.
// Only populated (and only validated) when the auth transport is "nats".
type NATSConfig struct {
	URL                string
	Subject            string
	ReconnectWaitMS    int
	ReconnectMaxWaitMS int
}

// Load assembles the Config from environment variables, applying defaults
// for optional values and validating the rest. Returns an error naming the
// offending variable on the first invalid or missing required value — the
// process is expected to exit rather than run half-configured. The auth
// transport is auto-detected: AUTH_SERVICE_URL selects h2c and wins over
// NATS_URL; one of the two must be set.
func Load() (*Config, error) {
	return load(true)
}

// LoadForSweep loads the storage, database, imaging, and ingest settings used by one-shot
// maintenance commands without requiring an authentication transport. Sweeps never serve requests
// or authorize callers, so requiring AUTH_SERVICE_URL or NATS_URL would add an unused deployment
// dependency.
func LoadForSweep() (*Config, error) {
	return load(false)
}

func load(requireAuthTransport bool) (*Config, error) {
	natsURL := getenv("NATS_URL", "")
	authServiceURL := getenv("AUTH_SERVICE_URL", "")

	var authTransport string
	if requireAuthTransport {
		var err error
		authTransport, err = resolveAuthTransport(authServiceURL, natsURL)
		if err != nil {
			return nil, err
		}
	}

	dbCfg, err := loadDatabaseConfig()
	if err != nil {
		return nil, err
	}

	port, err := getenvPort("PORT", 4003)
	if err != nil {
		return nil, err
	}

	maxAgeSecs, err := getenvInt("DOCUMENT_MAX_AGE", 86400)
	if err != nil {
		return nil, fmt.Errorf("DOCUMENT_MAX_AGE: %w", err)
	}
	if maxAgeSecs < 0 {
		return nil, fmt.Errorf("DOCUMENT_MAX_AGE must be >= 0")
	}

	breakerCfg, err := loadBreakerConfig()
	if err != nil {
		return nil, err
	}

	var natsCfg NATSConfig
	if authTransport == "nats" {
		natsCfg, err = loadNATSConfig(natsURL)
		if err != nil {
			return nil, err
		}
	}

	ingestCfg, err := loadIngestConfig()
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:           port,
		StoragePath:    getenv("LOCAL_STORAGE_PATH", "../server/.storage"),
		StorageType:    getenv("STORAGE_TYPE", "local"),
		DocumentMaxAge: time.Duration(maxAgeSecs) * time.Second,
		AuthTransport:  authTransport,
		AuthServiceURL: authServiceURL,
		Breaker:        breakerCfg,
		AlkemioDB:      dbCfg,
		NATS:           natsCfg,
		Ingest:         ingestCfg,
		BackupOutbox: BackupOutboxConfig{
			Enabled:         getenvBool("FILE_BACKUP_OUTBOX_ENABLED", false),
			HotMimePrefixes: getenvCSV("FILE_BACKUP_HOT_MIME_PREFIXES", defaultHotMimePrefixes),
			DoneRetention:   getenvHours("FILE_BACKUP_OUTBOX_DONE_RETENTION_HOURS", 24*time.Hour),
		},
		SweepCIDs: SweepCIDsConfig{
			DefaultRate: defaultSweepCIDsRate,
			ReportDir:   sweepCIDsReportDir,
		},
	}, nil
}

func loadIngestConfig() (IngestConfig, error) {
	maxUpload, err := getenvInt("MAX_UPLOAD_SIZE", 32<<20)
	if err != nil {
		return IngestConfig{}, fmt.Errorf("MAX_UPLOAD_SIZE: %w", err)
	}
	if maxUpload <= 0 {
		return IngestConfig{}, fmt.Errorf("MAX_UPLOAD_SIZE must be positive")
	}
	if maxUpload > 1<<30 {
		// 1 GiB is the validated ceiling of the streaming pipeline
		// (spec 020 FR-005/SC-001); larger values are unproven territory.
		return IngestConfig{}, fmt.Errorf("MAX_UPLOAD_SIZE must be <= 1073741824 (1 GiB, the validated ceiling)")
	}

	idleMS, err := getenvInt("UPLOAD_IDLE_TIMEOUT_MS", 30000)
	if err != nil {
		return IngestConfig{}, fmt.Errorf("UPLOAD_IDLE_TIMEOUT_MS: %w", err)
	}
	if idleMS <= 0 {
		return IngestConfig{}, fmt.Errorf("UPLOAD_IDLE_TIMEOUT_MS must be positive")
	}

	pixelBudget, err := getenvInt("IMAGE_PIXEL_BUDGET", 100_000_000)
	if err != nil {
		return IngestConfig{}, fmt.Errorf("IMAGE_PIXEL_BUDGET: %w", err)
	}
	if pixelBudget <= 0 {
		return IngestConfig{}, fmt.Errorf("IMAGE_PIXEL_BUDGET must be positive")
	}

	discThreshold, err := getenvInt("VIPS_STREAM_DISC_THRESHOLD", 0)
	if err != nil {
		return IngestConfig{}, fmt.Errorf("VIPS_STREAM_DISC_THRESHOLD: %w", err)
	}
	if discThreshold < 0 {
		return IngestConfig{}, fmt.Errorf("VIPS_STREAM_DISC_THRESHOLD must be >= 0")
	}

	pipeLimit, err := getenvInt("VIPS_PIPE_READ_LIMIT", 0)
	if err != nil {
		return IngestConfig{}, fmt.Errorf("VIPS_PIPE_READ_LIMIT: %w", err)
	}
	if pipeLimit < 0 {
		return IngestConfig{}, fmt.Errorf("VIPS_PIPE_READ_LIMIT must be >= 0")
	}

	return IngestConfig{
		MaxUploadSize:     int64(maxUpload),
		IdleTimeout:       time.Duration(idleMS) * time.Millisecond,
		PixelBudget:       int64(pixelBudget),
		VipsDiscThreshold: int64(discThreshold),
		VipsScratchDir:    getenv("VIPS_SCRATCH_DIR", ""),
		VipsPipeReadLimit: int64(pipeLimit),
	}, nil
}

func resolveAuthTransport(authServiceURL, natsURL string) (string, error) {
	switch {
	case authServiceURL != "":
		return "h2c", nil
	case natsURL != "":
		return "nats", nil
	default:
		return "", fmt.Errorf("either AUTH_SERVICE_URL or NATS_URL must be set")
	}
}

func loadDatabaseConfig() (DatabaseConfig, error) {
	dbHost := getenv("ALKEMIO_DATABASE_HOST", "")
	if dbHost == "" {
		return DatabaseConfig{}, fmt.Errorf("ALKEMIO_DATABASE_HOST is required")
	}

	dbPort, err := getenvPort("ALKEMIO_DATABASE_PORT", 5432)
	if err != nil {
		return DatabaseConfig{}, err
	}

	dbUser := getenv("ALKEMIO_DATABASE_USERNAME", "")
	if dbUser == "" {
		return DatabaseConfig{}, fmt.Errorf("ALKEMIO_DATABASE_USERNAME is required")
	}

	dbPass := getenv("ALKEMIO_DATABASE_PASSWORD", "")
	if dbPass == "" {
		return DatabaseConfig{}, fmt.Errorf("ALKEMIO_DATABASE_PASSWORD is required")
	}

	dbName := getenv("ALKEMIO_DATABASE_NAME", "")
	if dbName == "" {
		return DatabaseConfig{}, fmt.Errorf("ALKEMIO_DATABASE_NAME is required")
	}

	return DatabaseConfig{
		Host:     dbHost,
		Port:     dbPort,
		Username: dbUser,
		Password: dbPass,
		Name:     dbName,
	}, nil
}

func loadBreakerConfig() (BreakerConfig, error) {
	failureThreshold, err := getenvInt("AUTH_BREAKER_FAILURE_THRESHOLD", 3)
	if err != nil {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_FAILURE_THRESHOLD: %w", err)
	}
	if failureThreshold < 1 {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_FAILURE_THRESHOLD must be positive")
	}

	breakerTimeout, err := getenvInt("AUTH_BREAKER_TIMEOUT_SECONDS", 15)
	if err != nil {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_TIMEOUT_SECONDS: %w", err)
	}
	if breakerTimeout < 5 {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_TIMEOUT_SECONDS must be >= 5")
	}

	halfOpen, err := getenvInt("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", 2)
	if err != nil {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS: %w", err)
	}
	if halfOpen < 1 {
		return BreakerConfig{}, fmt.Errorf("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS must be positive")
	}

	return BreakerConfig{
		FailureThreshold:    failureThreshold,
		TimeoutSecs:         breakerTimeout,
		HalfOpenMaxRequests: halfOpen,
	}, nil
}

func loadNATSConfig(natsURL string) (NATSConfig, error) {
	reconnectWait, err := getenvInt("NATS_RECONNECT_WAIT_MS", 1000)
	if err != nil {
		return NATSConfig{}, fmt.Errorf("NATS_RECONNECT_WAIT_MS: %w", err)
	}
	if reconnectWait < 1 {
		return NATSConfig{}, fmt.Errorf("NATS_RECONNECT_WAIT_MS must be positive")
	}

	reconnectMax, err := getenvInt("NATS_RECONNECT_MAX_WAIT_MS", 30000)
	if err != nil {
		return NATSConfig{}, fmt.Errorf("NATS_RECONNECT_MAX_WAIT_MS: %w", err)
	}
	if reconnectMax < 1 {
		return NATSConfig{}, fmt.Errorf("NATS_RECONNECT_MAX_WAIT_MS must be positive")
	}
	if reconnectMax < reconnectWait {
		return NATSConfig{}, fmt.Errorf("NATS_RECONNECT_MAX_WAIT_MS (%d) must be >= NATS_RECONNECT_WAIT_MS (%d)", reconnectMax, reconnectWait)
	}

	return NATSConfig{
		URL:                natsURL,
		Subject:            getenv("NATS_SUBJECT", "auth.evaluate"),
		ReconnectWaitMS:    reconnectWait,
		ReconnectMaxWaitMS: reconnectMax,
	}, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getenvBool reads a boolean env var (strconv.ParseBool: 1/t/true/0/f/false…); an unset or
// blank value returns the fallback, and an unparseable value ALSO returns the fallback (a
// mis-typed flag must not silently flip a default the wrong way — it stays at the default).
func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// getenvHours reads an integer-hours env var into a Duration; unset, non-numeric, or
// non-positive returns the fallback (a mis-typed value stays at the default, like getenvBool).
func getenvHours(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Hour
}

// getenvCSV reads a comma-separated env var into a trimmed, non-empty slice; unset returns the
// fallback (so ops can override the whole list but the default is used when the var is absent).
func getenvCSV(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func getenvPort(key string, fallback int) (int, error) {
	v, err := getenvInt(key, fallback)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if v < 1 || v > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", key)
	}
	return v, nil
}

func getenvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	return n, nil
}

// LoadedSweepCIDsReportDir exposes the reserved report directory name so the
// storage adapter — which owns the key rule that makes the name collision-proof —
// can assert the two have not drifted. Adapters depend on config, not the
// reverse, so this is the only direction the check can run in.
func LoadedSweepCIDsReportDir() string { return sweepCIDsReportDir }
