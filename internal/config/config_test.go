package config

import (
	"testing"
)

func TestLoad_MissingRequired(t *testing.T) {
	// Clear all env vars
	for _, key := range []string{"AUTH_SERVICE_URL", "NATS_URL", "ALKEMIO_DATABASE_HOST", "ALKEMIO_DATABASE_USERNAME", "ALKEMIO_DATABASE_PASSWORD", "ALKEMIO_DATABASE_NAME"} {
		t.Setenv(key, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing NATS_URL")
	}
}

func TestLoad_MinimalValid(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "testdb")

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
		t.Errorf("NATS.FailureThreshold = %d", cfg.Breaker.FailureThreshold)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
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
		t.Errorf("NATS.FailureThreshold = %d", cfg.Breaker.FailureThreshold)
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "testdb")
	t.Setenv("PORT", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid PORT")
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("AUTH_SERVICE_URL", "")
		t.Setenv("NATS_URL", "nats://localhost:4222")
		t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
		t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
		t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
		t.Setenv("ALKEMIO_DATABASE_NAME", "testdb")
	}

	t.Run("DBPortOutOfRange", func(t *testing.T) {
		base(t)
		t.Setenv("ALKEMIO_DATABASE_PORT", "99999")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("AppPortOutOfRange", func(t *testing.T) {
		base(t)
		t.Setenv("PORT", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("NegativeMaxAge", func(t *testing.T) {
		base(t)
		t.Setenv("DOCUMENT_MAX_AGE", "-1")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ReconnectWaitZero", func(t *testing.T) {
		base(t)
		t.Setenv("NATS_RECONNECT_WAIT_MS", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ReconnectMaxZero", func(t *testing.T) {
		base(t)
		t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ReconnectMaxLessThanWait", func(t *testing.T) {
		base(t)
		t.Setenv("NATS_RECONNECT_WAIT_MS", "5000")
		t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "1000")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("FailureThresholdZero", func(t *testing.T) {
		base(t)
		t.Setenv("AUTH_BREAKER_FAILURE_THRESHOLD", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("BreakerTimeoutTooLow", func(t *testing.T) {
		base(t)
		t.Setenv("AUTH_BREAKER_TIMEOUT_SECONDS", "2")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("HalfOpenZero", func(t *testing.T) {
		base(t)
		t.Setenv("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", "0")
		_, err := Load()
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestLoad_MissingDBHost(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DB host")
	}
}

func TestLoad_MissingDBUsername(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DB username")
	}
}

func TestLoad_MissingDBPassword(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DB password")
	}
}

func TestLoad_MissingDBName(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DB name")
	}
}

func TestLoad_InvalidDBPort(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_PORT", "abc")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "db")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid DB port")
	}
}

func TestLoad_InvalidDocumentMaxAge(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "db")
	t.Setenv("DOCUMENT_MAX_AGE", "not-int")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid max age")
	}
}

func TestLoad_InvalidReconnectWait(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "db")
	t.Setenv("NATS_RECONNECT_WAIT_MS", "xyz")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad_InvalidReconnectMax(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "db")
	t.Setenv("NATS_RECONNECT_MAX_WAIT_MS", "xyz")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLoad_InvalidHalfOpen(t *testing.T) {
	t.Setenv("AUTH_SERVICE_URL", "")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("ALKEMIO_DATABASE_HOST", "localhost")
	t.Setenv("ALKEMIO_DATABASE_USERNAME", "user")
	t.Setenv("ALKEMIO_DATABASE_PASSWORD", "pass")
	t.Setenv("ALKEMIO_DATABASE_NAME", "db")
	t.Setenv("AUTH_BREAKER_HALF_OPEN_MAX_REQUESTS", "xyz")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
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
}
