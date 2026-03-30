package config

import (
	"fmt"
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
}

type DatabaseConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	Name     string
}

func (d DatabaseConfig) ConnString() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		d.Username, d.Password, d.Host, d.Port, d.Name)
}

type NATSConfig struct {
	URL                 string
	Subject             string
	ReconnectWaitMS     int
	ReconnectMaxWaitMS  int
	FailureThreshold    int
	BreakerTimeoutSecs  int
	HalfOpenMaxRequests int
}

func Load() (*Config, error) {
	natsURL := getenv("NATS_URL", "")
	if natsURL == "" {
		return nil, fmt.Errorf("NATS_URL is required")
	}

	dbHost := getenv("ALKEMIO_DATABASE_HOST", "")
	if dbHost == "" {
		return nil, fmt.Errorf("ALKEMIO_DATABASE_HOST is required")
	}

	dbPort, err := getenvInt("ALKEMIO_DATABASE_PORT", 5432)
	if err != nil {
		return nil, fmt.Errorf("ALKEMIO_DATABASE_PORT: %w", err)
	}

	dbUser := getenv("ALKEMIO_DATABASE_USERNAME", "")
	if dbUser == "" {
		return nil, fmt.Errorf("ALKEMIO_DATABASE_USERNAME is required")
	}

	dbPass := getenv("ALKEMIO_DATABASE_PASSWORD", "")
	if dbPass == "" {
		return nil, fmt.Errorf("ALKEMIO_DATABASE_PASSWORD is required")
	}

	dbName := getenv("ALKEMIO_DATABASE_NAME", "")
	if dbName == "" {
		return nil, fmt.Errorf("ALKEMIO_DATABASE_NAME is required")
	}

	port, err := getenvInt("PORT", 4003)
	if err != nil {
		return nil, fmt.Errorf("PORT: %w", err)
	}

	maxAgeSecs, err := getenvInt("DOCUMENT_MAX_AGE", 86400)
	if err != nil {
		return nil, fmt.Errorf("DOCUMENT_MAX_AGE: %w", err)
	}

	reconnectWait, err := getenvInt("NATS_RECONNECT_WAIT_MS", 1000)
	if err != nil {
		return nil, fmt.Errorf("NATS_RECONNECT_WAIT_MS: %w", err)
	}

	reconnectMax, err := getenvInt("NATS_RECONNECT_MAX_WAIT_MS", 30000)
	if err != nil {
		return nil, fmt.Errorf("NATS_RECONNECT_MAX_WAIT_MS: %w", err)
	}
	if reconnectMax < reconnectWait {
		return nil, fmt.Errorf("NATS_RECONNECT_MAX_WAIT_MS (%d) must be >= NATS_RECONNECT_WAIT_MS (%d)", reconnectMax, reconnectWait)
	}

	failureThreshold, err := getenvInt("NATS_FAILURE_THRESHOLD", 3)
	if err != nil {
		return nil, fmt.Errorf("NATS_FAILURE_THRESHOLD: %w", err)
	}
	if failureThreshold < 1 {
		return nil, fmt.Errorf("NATS_FAILURE_THRESHOLD must be positive")
	}

	breakerTimeout, err := getenvInt("NATS_BREAKER_TIMEOUT_SECONDS", 15)
	if err != nil {
		return nil, fmt.Errorf("NATS_BREAKER_TIMEOUT_SECONDS: %w", err)
	}
	if breakerTimeout < 5 {
		return nil, fmt.Errorf("NATS_BREAKER_TIMEOUT_SECONDS must be >= 5")
	}

	halfOpen, err := getenvInt("NATS_HALF_OPEN_MAX_REQUESTS", 2)
	if err != nil {
		return nil, fmt.Errorf("NATS_HALF_OPEN_MAX_REQUESTS: %w", err)
	}
	if halfOpen < 1 {
		return nil, fmt.Errorf("NATS_HALF_OPEN_MAX_REQUESTS must be positive")
	}

	return &Config{
		Port:           port,
		StoragePath:    getenv("LOCAL_STORAGE_PATH", "../server/.storage"),
		StorageType:    getenv("STORAGE_TYPE", "local"),
		DocumentMaxAge: time.Duration(maxAgeSecs) * time.Second,
		AlkemioDB: DatabaseConfig{
			Host:     dbHost,
			Port:     dbPort,
			Username: dbUser,
			Password: dbPass,
			Name:     dbName,
		},
		NATS: NATSConfig{
			URL:                 natsURL,
			Subject:             getenv("NATS_SUBJECT", "auth.evaluate"),
			ReconnectWaitMS:     reconnectWait,
			ReconnectMaxWaitMS:  reconnectMax,
			FailureThreshold:    failureThreshold,
			BreakerTimeoutSecs:  breakerTimeout,
			HalfOpenMaxRequests: halfOpen,
		},
	}, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
