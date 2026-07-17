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
	// UpdateMetadataWithOutbox applies the versioned metadata update AND enqueues a backup-outbox
	// row for the now-durable document atomically (same commit), then NOTIFYs. Used when a PATCH
	// transitions a document temporary→durable (013 conversation media reaches durability via a
	// temporaryLocation:true→false flip — the re-home MOVE / re-share pin / outbound flip — never
	// via create/replace, so this is the only path that can enqueue its backup hint). Same error
	// semantics as DocumentRepo.UpdateMetadata (0 rows → model.ErrDocumentNotFound, which the
	// service maps to ErrConflict; a unique violation → model.ErrDuplicateKey).
	UpdateMetadataWithOutbox(ctx context.Context, id uuid.UUID, meta model.DocumentMetadataUpdate, version int, externalID string, size int, priority int16) error
	// PruneBackupOutbox drops `done` outbox rows older than the cutoff, keeping the shared
	// outbox bounded (SC-008); returns the number pruned.
	PruneBackupOutbox(ctx context.Context, olderThan time.Time) (int64, error)
}
