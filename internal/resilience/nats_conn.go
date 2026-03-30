package resilience

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service-go/internal/config"
)

// ConnectNATS establishes a NATS connection with exponential backoff reconnect.
func ConnectNATS(cfg config.NATSConfig, logger *zap.Logger) (*nats.Conn, error) {
	baseWait := time.Duration(cfg.ReconnectWaitMS) * time.Millisecond
	maxWait := time.Duration(cfg.ReconnectMaxWaitMS) * time.Millisecond
	jit := baseWait / 2

	opts := []nats.Option{
		nats.Name("file-service"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(baseWait),
		nats.ReconnectJitter(jit, jit),
		nats.CustomReconnectDelay(func(attempts int) time.Duration {
			return calculateReconnectDelay(baseWait, maxWait, attempts)
		}),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("NATS disconnected", zap.Error(err))
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			logger.Info("NATS reconnected")
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			logger.Warn("NATS connection closed")
		}),
	}

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, err
	}

	logger.Info("NATS connected", zap.String("url", cfg.URL))
	return nc, nil
}

func calculateReconnectDelay(base, maxDelay time.Duration, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	multiplier := math.Pow(2, float64(attempts-1))
	delayNs := float64(base) * multiplier
	if delayNs > float64(maxDelay) || delayNs < 0 {
		delayNs = float64(maxDelay)
	}
	delay := time.Duration(delayNs)
	if jitterRange := int64(delay / 4); jitterRange > 0 {
		delay += time.Duration(rand.Int64N(jitterRange)) //nolint:gosec // jitter, not security
	}
	return delay
}
