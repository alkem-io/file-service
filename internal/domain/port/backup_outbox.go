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
	// UpdateFileWithOutbox replaces a document's content and, when the row is DURABLE at
	// replace-commit time, enqueues a backup hint for the new content hash atomically. The
	// durability decision is read in-tx from the UPDATE's RETURNING "temporaryLocation" (the
	// row while UPDATE-locked), NOT a caller snapshot: a replace that loaded the doc as temporary
	// can race a concurrent temp→durable PATCH that commits first, so the snapshot could be stale
	// and strand the now-durable content with no outbox row. Returns whether a backup row was
	// enqueued (true only on a durable replace). Same error semantics as DocumentRepo.UpdateFile.
	UpdateFileWithOutbox(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata, priority int16) (enqueued bool, err error)
	// UpdateMetadataWithOutbox applies the versioned metadata update AND enqueues a backup-outbox
	// row for the now-durable document atomically (same commit), then NOTIFYs. Used when a PATCH
	// transitions a document temporary→durable (013 conversation media reaches durability via a
	// temporaryLocation:true→false flip — the re-home MOVE / re-share pin / outbound flip — never
	// via create/replace, so this is the only path that can enqueue its backup hint). Same error
	// semantics as DocumentRepo.UpdateMetadata (0 rows → model.ErrDocumentNotFound, which the
	// service maps to ErrConflict; a unique violation → model.ErrDuplicateKey).
	//
	// The enqueued externalID/size are read from the row while it is UPDATE-locked inside this
	// transaction (full-row RETURNING), NOT passed in by the caller: a concurrent content-replace
	// swaps externalID/size WITHOUT bumping version, so a handler-threaded snapshot could be stale
	// and leave the durable content with no outbox row (an RPO gap). The whole AUTHORITATIVE
	// post-update document is returned so the caller can build the PATCH response from one
	// consistent snapshot without a re-read. Priority is likewise computed in-tx from the
	// AUTHORITATIVE post-update mime via the caller-supplied priorityFor: a concurrent
	// content-replace can upgrade a still-temporary row's mime (generic→concrete, non-hot→hot)
	// WITHOUT bumping version, so deriving priority from a handler-threaded pre-update snapshot
	// could enqueue a now-hot object at normal priority (behind the RPO budget hot protects) —
	// computing it from the RETURNING row's mime closes that gap.
	UpdateMetadataWithOutbox(ctx context.Context, id uuid.UUID, meta model.DocumentMetadataUpdate, version int, priorityFor func(mimeType string) int16) (model.Document, error)
	// PruneBackupOutbox drops `done` outbox rows older than the cutoff, keeping the shared
	// outbox bounded (SC-008); returns the number pruned.
	PruneBackupOutbox(ctx context.Context, olderThan time.Time) (int64, error)
}
