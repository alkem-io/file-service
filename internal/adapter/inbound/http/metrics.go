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

	// ReplaceOutcomes counts content-replace results by outcome:
	// accepted | fallback_generic_sniff | rejected_empty | rejected_mismatch.
	// The rejected_* keys are the alertable signal (spec 019 FR-008): a spike
	// means an editor integration broke, not user error.
	ReplaceOutcomes = expvar.NewMap("content_replace_outcomes_total")

	// MimeRepairOps counts startup MIME-repair actions:
	// relabeled | unrecoverable | skipped_not_office | errors.
	MimeRepairOps = expvar.NewMap("mime_repair_total")

	// IngestOutcomes counts streaming-upload results by outcome (spec 020
	// FR-008): accepted | rejected_over_limit | rejected_pixel_budget |
	// rejected_bucket_policy | stalled | client_abort | failed_mid_stream.
	// stalled/client_abort/failed_mid_stream distinguish who broke the
	// stream; the rejected_* keys are policy enforcement.
	IngestOutcomes = expvar.NewMap("ingest_outcomes_total")
)

// InitMetrics ensures metrics are initialized (idempotent).
func InitMetrics() {
	metricsOnce.Do(func() {
		NATSConnectionState.Set("disconnected")
		StorageOps.Init()
		DocumentOps.Init()
		ReplaceOutcomes.Init()
		MimeRepairOps.Init()
		IngestOutcomes.Init()
	})
}
