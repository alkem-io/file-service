package http

import (
	"expvar"
	"sync"
)

var (
	metricsOnce sync.Once

	NATSConnectionState = expvar.NewString("resilience_nats_connection_state")
	NATSReconnects      = expvar.NewInt("resilience_nats_reconnect_attempts")
	NATSDisconnects     = expvar.NewInt("resilience_nats_disconnects")
	BreakerState        = expvar.NewMap("resilience_breaker_state")
	StorageOps          = expvar.NewMap("storage_operations_total")
	DocumentOps         = expvar.NewMap("document_operations_total")
)

// InitMetrics ensures metrics are initialized (idempotent).
func InitMetrics() {
	metricsOnce.Do(func() {
		NATSConnectionState.Set("disconnected")
		StorageOps.Init()
		DocumentOps.Init()
	})
}
