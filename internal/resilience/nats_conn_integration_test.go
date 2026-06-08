package resilience

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/config"
)

func TestConnectNATS_Success(t *testing.T) {
	srv := startEmbeddedNATS(t)
	defer srv.Shutdown()

	logger, _ := zap.NewDevelopment()

	cfg := config.NATSConfig{
		URL:                srv.ClientURL(),
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 1000,
	}

	nc, err := ConnectNATS(cfg, logger)
	if err != nil {
		t.Fatalf("ConnectNATS: %v", err)
	}
	defer nc.Close()

	if !nc.IsConnected() {
		t.Error("expected connected")
	}
}

func TestConnectNATS_BadURL(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	cfg := config.NATSConfig{
		URL:                "nats://127.0.0.1:1", // nothing listening
		ReconnectWaitMS:    100,
		ReconnectMaxWaitMS: 200,
	}

	_, err := ConnectNATS(cfg, logger)
	if err == nil {
		t.Fatal("expected error for bad URL")
	}
}

func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("NATS not ready")
	}
	return srv
}
