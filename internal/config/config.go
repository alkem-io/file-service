package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

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
}

// BreakerConfig is shared circuit breaker settings for any auth transport.
type BreakerConfig struct {
	FailureThreshold    int
	TimeoutSecs         int
	HalfOpenMaxRequests int
}

type DatabaseConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	Name     string
}

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

type NATSConfig struct {
	URL                string
	Subject            string
	ReconnectWaitMS    int
	ReconnectMaxWaitMS int
}

func Load() (*Config, error) {
	natsURL := getenv("NATS_URL", "")
	authServiceURL := getenv("AUTH_SERVICE_URL", "")

	authTransport, err := resolveAuthTransport(authServiceURL, natsURL)
	if err != nil {
		return nil, err
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
