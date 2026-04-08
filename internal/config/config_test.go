package config

import (
	"strings"
	"testing"
)

func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_PORT", "")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "testdb")
	t.Setenv("PORT", "")
	t.Setenv("DOCUMENT_MAX_AGE", "")
	t.Setenv("LOCAL_STORAGE_PATH", "")
	t.Setenv("STORAGE_TYPE", "")
	t.Setenv("AUTH_BREAKER_FAILURE_THRESHOLD", "")
	t.Setenv("AUTH_BREAKER_TIMEOUT_SECONDS", "")
	t.Setenv("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", "")
	t.Setenv("NATS_SUBJECT", "")
	t.Setenv("NATS_RECONNECT_WAIT_MS", "")
	t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "")
}

func requireLoadErr(t *testing.T, want string) {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	t.Run("NoAuthTransport", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTH_SERVICE_URL", "")
		t.Setenv("NATS_URL", "")
		requireLoadErr(t, "AUTH_SERVICE_URL")
	})

	for _, key := range []string{
		"ALKEMIO_DATABASE_HOST",
		"ALKEMIO_DATABASE_USERNAME",
		"ALKEMIO_DATABASE_PASSWORD",
		"ALKEMIO_DATABASE_NAME",
	} {
		t.Run("Missing_"+key, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv(key, "")
			requireLoadErr(t, key)
		})
	}
}

func TestLoad_MinimalValid(t *testing.T) {
	setBaseEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 4003 {
		t.Errorf("Port = %d, want 4003", cfg.Port)
	}
	if cfg.StoragePath != "../server/.storage" {
		t.Errorf("StoragePath = %q", cfg.StoragePath)
	}
	if cfg.StorageType != "local" {
		t.Errorf("StorageType = %q", cfg.StorageType)
	}
	if cfg.DocumentMaxAge.Seconds() != 86400 {
		t.Errorf("DocumentMaxAge = %v", cfg.DocumentMaxAge)
	}
	if cfg.NATS.Subject != "auth.evaluate" {
		t.Errorf("NATS.Subject = %q", cfg.NATS.Subject)
	}
	if cfg.NATS.ReconnectWaitMS != 1000 {
		t.Errorf("NATS.ReconnectWaitMS = %d", cfg.NATS.ReconnectWaitMS)
	}
	if cfg.Breaker.FailureThreshold != 3 {
		t.Errorf("Breaker.FailureThreshold = %d", cfg.Breaker.FailureThreshold)
	}
	if cfg.AuthTransport != "nats" {
		t.Errorf("AuthTransport = %q, want %q", cfg.AuthTransport, "nats")
	}
}

func TestLoad_CustomValues(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("NATS_URL", "nats://custom:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "dbhost")
	t.Setenv("ALKEMIO_DATABASE_PORT", "5433")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "mydb")
	t.Setenv("PORT", "8080")
	t.Setenv("LOCAL_STORAGE_PATH", "/data/files")
	t.Setenv("DOCUMENT_MAX_AGE", "3600")
	t.Setenv("NATS_SUBJECT", "custom.subject")
	t.Setenv("AUTH_BREAKER_FAILURE_THRESHOLD", "5")
	t.Setenv("AUTH_BREAKER_TIMEOUT_SECONDS", "30")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("Port = %d", cfg.Port)
	}
	if cfg.StoragePath != "/data/files" {
		t.Errorf("StoragePath = %q", cfg.StoragePath)
	}
	if cfg.DocumentMaxAge.Seconds() != 3600 {
		t.Errorf("DocumentMaxAge = %v", cfg.DocumentMaxAge)
	}
	if cfg.AlkemioDB.Port != 5433 {
		t.Errorf("DB Port = %d", cfg.AlkemioDB.Port)
	}
	if cfg.NATS.Subject != "custom.subject" {
		t.Errorf("NATS.Subject = %q", cfg.NATS.Subject)
	}
	if cfg.Breaker.FailureThreshold != 5 {
		t.Errorf("Breaker.FailureThreshold = %d", cfg.Breaker.FailureThreshold)
	}
	if cfg.Breaker.TimeoutSecs != 30 {
		t.Errorf("Breaker.TimeoutSecs = %d", cfg.Breaker.TimeoutSecs)
	}
}

func TestLoad_H2CTransport(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("AUTH_SERVICE_URL", "http://auth-service:4000")
	t.Setenv("NATS_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthTransport != "h2c" {
		t.Errorf("AuthTransport = %q, want %q", cfg.AuthTransport, "h2c")
	}
	if cfg.AuthServiceURL != "http://auth-service:4000" {
		t.Errorf("AuthServiceURL = %q", cfg.AuthServiceURL)
	}
	if cfg.NATS.URL != "" {
		t.Errorf("NATS.URL = %q, want empty for h2c transport", cfg.NATS.URL)
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	t.Run("InvalidPort", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("PORT", "not-a-number")
		requireLoadErr(t, "PORT")
	})

	t.Run("DBPortOutOfRange", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_PORT", "99999")
		requireLoadErr(t, "ALKEMIO_DATABASE_PORT")
	})

	t.Run("AppPortOutOfRange", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("PORT", "0")
		requireLoadErr(t, "PORT")
	})

	t.Run("NegativeMaxAge", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("DOCUMENT_MAX_AGE", "-1")
		requireLoadErr(t, "DOCUMENT_MAX_AGE")
	})

	t.Run("ReconnectWaitZero", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("NATS_RECONNECT_WAIT_MS", "0")
		requireLoadErr(t, "NATS_RECONNECT_WAIT_MS")
	})

	t.Run("ReconnectMaxZero", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "0")
		requireLoadErr(t, "NATS_RECONNECT_MAX_WAIT_MS")
	})

	t.Run("ReconnectMaxLessThanWait", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("NATS_RECONNECT_WAIT_MS", "5000")
		t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "1000")
		requireLoadErr(t, "NATS_RECONNECT_MAX_WAIT_MS")
	})

	t.Run("FailureThresholdZero", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTH_BREAKER_FAILURE_THRESHOLD", "0")
		requireLoadErr(t, "AUTH_BREAKER_FAILURE_THRESHOLD")
	})

	t.Run("BreakerTimeoutTooLow", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTH_BREAKER_TIMEOUT_SECONDS", "2")
		requireLoadErr(t, "AUTH_BREAKER_TIMEOUT_SECONDS")
	})

	t.Run("HalfOpenZero", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", "0")
		requireLoadErr(t, "AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS")
	})

	t.Run("MissingDBHost", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_HOST", "")
		requireLoadErr(t, "ALKEMIO_DATABASE_HOST")
	})

	t.Run("MissingDBUsername", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_USERNAME", "")
		requireLoadErr(t, "ALKEMIO_DATABASE_USERNAME")
	})

	t.Run("MissingDBPassword", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_PASSWORD", "")
		requireLoadErr(t, "ALKEMIO_DATABASE_PASSWORD")
	})

	t.Run("MissingDBName", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_NAME", "")
		requireLoadErr(t, "ALKEMIO_DATABASE_NAME")
	})

	t.Run("InvalidDBPort", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ALKEMIO_DATABASE_PORT", "abc")
		requireLoadErr(t, "ALKEMIO_DATABASE_PORT")
	})

	t.Run("InvalidDocumentMaxAge", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("DOCUMENT_MAX_AGE", "not-int")
		requireLoadErr(t, "DOCUMENT_MAX_AGE")
	})

	t.Run("InvalidReconnectWait", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("NATS_RECONNECT_WAIT_MS", "xyz")
		requireLoadErr(t, "NATS_RECONNECT_WAIT_MS")
	})

	t.Run("InvalidReconnectMax", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "xyz")
		requireLoadErr(t, "NATS_RECONNECT_MAX_WAIT_MS")
	})

	t.Run("InvalidHalfOpen", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", "xyz")
		requireLoadErr(t, "AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS")
	})
}

func TestNewLogger(t *testing.T) {
	logger, err := NewLogger()
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("nil logger")
	}
}

func TestConnString(t *testing.T) {
	db := DatabaseConfig{
		Host:     "myhost",
		Port:     5432,
		Username: "user",
		Password: "pass", //nolint:gosec // test credentials
		Name:     "mydb",
	}
	got := db.ConnString()
	want := "postgres://user:pass@myhost:5432/mydb?sslmode=disable" //nolint:gosec // test expected value
	if got != want {
		t.Errorf("ConnString() = %q, want %q", got, want)
	}
}

func TestGetenv_Fallback(t *testing.T) {
	t.Setenv("TEST_GETENV_KEY", "")
	got := getenv("TEST_GETENV_KEY", "default")
	if got != "default" {
		t.Errorf("getenv = %q, want %q", got, "default")
	}
}

func TestGetenv_Set(t *testing.T) {
	t.Setenv("TEST_GETENV_KEY", "custom")
	got := getenv("TEST_GETENV_KEY", "default")
	if got != "custom" {
		t.Errorf("getenv = %q, want %q", got, "custom")
	}
}

func TestGetenvInt_Fallback(t *testing.T) {
	t.Setenv("TEST_INT_KEY", "")
	got, err := getenvInt("TEST_INT_KEY", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("getenvInt = %d, want 42", got)
	}
}

func TestGetenvInt_Valid(t *testing.T) {
	t.Setenv("TEST_INT_KEY", "99")
	got, err := getenvInt("TEST_INT_KEY", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != 99 {
		t.Errorf("getenvInt = %d, want 99", got)
	}
}

func TestGetenvInt_Invalid(t *testing.T) {
	t.Setenv("TEST_INT_KEY", "abc")
	_, err := getenvInt("TEST_INT_KEY", 42)
	if err == nil {
		t.Fatal("expected error for invalid int")
	}
	if !strings.Contains(err.Error(), "invalid integer") {
		t.Errorf("error = %q, want substring %q", err.Error(), "invalid integer")
	}
}
