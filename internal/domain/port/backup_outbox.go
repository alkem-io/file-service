package port

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// BackupOutboxRepo is the transactional producer for the continuous-backup outbox
// (008-continuous-file-backup). Its write methods commit a document row AND its backup-outbox
// row in ONE transaction (FR-001: no committed outbox entry without a file row), then emit
// NOTIFY. A nil BackupOutboxRepo on the FileService means the producer is OFF (the flag default)
// and the plain DocumentRepo path runs instead — so this port is a pure opt-in overlay on the
// existing write path, never a behaviour change when disabled.
type BackupOutboxRepo interface {
	// CreateWithOutbox inserts a new document and enqueues its backup hint atomically.
	// Returns model.ErrDuplicateKey on the dedup race (no outbox row written), exactly as
	// DocumentRepo.Create does, so the service's dedup path is unchanged.
	CreateWithOutbox(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata, priority int16) (uuid.UUID, error)
	// UpdateFileWithOutbox replaces a document's content and enqueues a backup hint for the new
	// content hash atomically. Same error semantics as DocumentRepo.UpdateFile.
	UpdateFileWithOutbox(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata, priority int16) error
	// PruneBackupOutbox drops `done` outbox rows older than the cutoff, keeping the shared
	// outbox bounded (SC-008); returns the number pruned.
	PruneBackupOutbox(ctx context.Context, olderThan time.Time) (int64, error)
	// DeletePendingByHash drops still-pending outbox rows for a content hash whose blob
	// file-service has just deleted (refcount→0) — orphan hygiene at the owner: those rows
	// point at bytes that no longer exist, so the consumer's fetch could only 404. Returns
	// the number removed.
	DeletePendingByHash(ctx context.Context, externalID string) (int64, error)
}
