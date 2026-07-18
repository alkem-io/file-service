package alkemiodb

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/adapter/outbound/alkemiodb/queries"
	"github.com/alkem-io/file-service/internal/domain/model"
)

// withOutboxTx runs the shared transactional-outbox protocol used by every backup-outbox
// producer method (008-continuous-file-backup FR-001): Begin, run the caller's row DML, run the
// caller's backup-outbox enqueue, Commit, then a best-effort NOTIFY. A failure in dml or enqueue
// (or Begin/Commit) rolls the tx back via the deferred Rollback with NO NOTIFY — so a file row and
// its outbox row always commit together or not at all. The dml closure owns its own error mapping
// (unique violation → model.ErrDuplicateKey, 0 rows / pgx.ErrNoRows → model.ErrDocumentNotFound)
// and may capture values the enqueue closure needs (e.g. the RETURNING externalID/size).
func (a *Adapter) withOutboxTx(ctx context.Context, dml, enqueue func(q *queries.Queries) error) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful Commit
	q := a.queries.WithTx(tx)

	if err := dml(q); err != nil {
		return err
	}
	if err := enqueue(q); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	a.notifyBackup(ctx)
	return nil
}

// CreateWithOutbox inserts a document row AND enqueues its backup-outbox row in ONE transaction
// (008-continuous-file-backup FR-001: no committed outbox entry without a file row, and every
// committed non-temporary file row carries its backup hint). After commit it emits a best-effort
// NOTIFY so the backup worker wakes immediately — the durable table + the worker's poll floor
// cover a lost NOTIFY. A unique violation on the document → model.ErrDuplicateKey with NO outbox
// row written (a dedup hit means the content already exists and was already captured); the
// service's dedup path re-queries the winner exactly as on the non-outbox path.
func (a *Adapter) CreateWithOutbox(ctx context.Context, doc model.Document, contentMetadata model.ContentMetadata, priority int16) (uuid.UUID, error) {
	raw, err := marshalContentMetadata(contentMetadata)
	if err != nil {
		return uuid.Nil, err
	}
	err = a.withOutboxTx(ctx,
		func(q *queries.Queries) error {
			// CreateDocument RETURNING id echoes the caller-generated doc.ID ($1); discard it.
			if _, err := q.CreateDocument(ctx, createDocumentParams(doc, raw)); err != nil {
				if isUniqueViolation(err) {
					return model.ErrDuplicateKey
				}
				return err
			}
			return nil
		},
		func(q *queries.Queries) error {
			return q.EnqueueBackupOutbox(ctx, queries.EnqueueBackupOutboxParams{
				FileId:      uuidToPgx(doc.ID),
				ExternalID:  doc.ExternalID,
				Priority:    priority,
				CreatedBy:   uuidToPgxNullable(doc.CreatedBy),
				CreatedDate: timeToPgx(doc.CreatedDate),
				Size:        int64(doc.Size),
			})
		},
	)
	if err != nil {
		return uuid.Nil, err
	}
	return doc.ID, nil
}

// UpdateFileWithOutbox rewrites a document's content fields AND — when the row is DURABLE at
// replace-commit time — enqueues a backup-outbox row for the NEW externalID in ONE transaction
// (a durable replace = a new content hash = a new object to back up). model.ErrDuplicateKey /
// model.ErrDocumentNotFound are surfaced exactly as UpdateFile does.
//
// The enqueue decision is made from the row's AUTHORITATIVE in-transaction "temporaryLocation" —
// the UpdateDocumentFile RETURNING value read while the row is UPDATE-locked here — NOT the
// caller's pre-loaded doc snapshot. A content-replace that loaded the doc as temporary can race a
// concurrent temp→durable PATCH that commits FIRST; the snapshot would then be stale and the
// replace would swap the durable row's content to a new hash with no outbox row (the stranded-blob
// race). Deciding in-tx closes it both ways: replace-before-flip (temporary → skip the enqueue, the
// later flip's own UpdateMetadataWithOutbox enqueues the then-current hash) and flip-before-replace
// (durable → enqueue the new hash). The returned bool reports whether a row was enqueued (so the
// service counts the metric only on a real enqueue). The outbox breadcrumb: createdBy is null
// (unknown on a replace) and createdDate is enqueue time (now) — the RPO-lag semantics the
// consumer's backlog gauge expects.
func (a *Adapter) UpdateFileWithOutbox(ctx context.Context, id uuid.UUID, externalID, mimeType string, size int, contentMetadata model.ContentMetadata, priority int16) (bool, error) {
	raw, err := marshalContentMetadata(contentMetadata)
	if err != nil {
		return false, err
	}
	var enqueued bool
	err = a.withOutboxTx(ctx,
		func(q *queries.Queries) error {
			temporaryLocation, err := q.UpdateDocumentFile(ctx, updateFileParams(id, externalID, mimeType, size, raw))
			if err != nil {
				if isUniqueViolation(err) {
					return model.ErrDuplicateKey
				}
				if errors.Is(err, pgx.ErrNoRows) {
					return model.ErrDocumentNotFound
				}
				return err
			}
			// Enqueue only when the row is durable at replace-commit time. A still-temporary row's
			// content isn't backed up yet; its later temporary→durable flip enqueues the then-current
			// hash via UpdateMetadataWithOutbox.
			enqueued = !temporaryLocation
			return nil
		},
		func(q *queries.Queries) error {
			if !enqueued {
				return nil
			}
			return q.EnqueueBackupOutbox(ctx, queries.EnqueueBackupOutboxParams{
				FileId:      uuidToPgx(id),
				ExternalID:  externalID,
				Priority:    priority,
				CreatedBy:   uuidToPgxNullable(nil), // no actor breadcrumb on a content replace
				CreatedDate: timeToPgxNow(),
				Size:        int64(size),
			})
		},
	)
	if err != nil {
		return false, err
	}
	return enqueued, nil
}

// UpdateMetadataWithOutbox applies the versioned PATCH metadata update AND enqueues a backup-outbox
// row for the now-durable document in ONE transaction (008-continuous-file-backup FR-001), then
// NOTIFYs. Called when a PATCH flips a document temporary→durable (013 conversation media reaches
// durability via a temporaryLocation:true→false flip — the re-home MOVE / re-share pin / outbound
// flip). model.ErrDuplicateKey / model.ErrDocumentNotFound are surfaced exactly as UpdateMetadata
// does.
//
// The enqueued externalID/size — and the whole returned document — come from the versioned UPDATE's
// full-row RETURNING clause: the row's AUTHORITATIVE post-update state read while it is UPDATE-locked
// in THIS transaction, NOT a caller-threaded snapshot. A concurrent content-replace swaps
// externalID/size (and dims) WITHOUT bumping version, so a snapshot taken by the handler before the
// update could be stale; reading it in-tx closes that RPO gap (the replace either committed first —
// RETURNING sees the new hash — or blocks on the row lock until this commits, then replaces on a
// now-durable row and enqueues its own outbox). The returned document lets the caller build the
// PATCH response from one consistent snapshot (no reload). The outbox breadcrumb: createdBy is the
// re-attributed owner (meta.CreatedBy, null when the PATCH leaves it unset) and createdDate is
// enqueue time (now) — the RPO-lag semantics the consumer's backlog gauge expects. Priority is
// derived from the SAME authoritative row via priorityFor(doc.MimeType): a concurrent replace can
// upgrade a still-temporary row's mime non-hot→hot without bumping version, so a pre-update snapshot
// would under-prioritize the now-durable hot object.
func (a *Adapter) UpdateMetadataWithOutbox(ctx context.Context, id uuid.UUID, meta model.DocumentMetadataUpdate, version int, priorityFor func(mimeType string) int16) (model.Document, error) {
	var doc model.Document
	err := a.withOutboxTx(ctx,
		func(q *queries.Queries) error {
			row, err := q.UpdateDocumentMetadata(ctx, updateMetadataParams(id, meta, version))
			if err != nil {
				if isUniqueViolation(err) {
					return model.ErrDuplicateKey
				}
				if errors.Is(err, pgx.ErrNoRows) {
					return model.ErrDocumentNotFound
				}
				return err
			}
			doc = updateMetaRowToDocument(row)
			return nil
		},
		func(q *queries.Queries) error {
			return q.EnqueueBackupOutbox(ctx, queries.EnqueueBackupOutboxParams{
				FileId:      uuidToPgx(id),
				ExternalID:  doc.ExternalID,                    // AUTHORITATIVE post-update hash from the locked row (RETURNING)
				Priority:    priorityFor(doc.MimeType),         // priority from the SAME authoritative mime, not a stale snapshot
				CreatedBy:   uuidToPgxNullable(meta.CreatedBy), // the re-attributed owner breadcrumb; nil if unset
				CreatedDate: timeToPgxNow(),
				Size:        int64(doc.Size),
			})
		},
	)
	if err != nil {
		return model.Document{}, err
	}
	return doc, nil
}

// PruneBackupOutbox deletes `done` outbox rows older than the cutoff, keeping the shared outbox
// bounded (SC-008). file-service owns the outbox DML; the durable record lives in the ledger.
func (a *Adapter) PruneBackupOutbox(ctx context.Context, olderThan time.Time) (int64, error) {
	return a.queries.PruneBackupOutboxDone(ctx, timeToPgx(olderThan))
}

// notifyBackup emits NOTIFY on the backup channel after a committed enqueue. Best-effort — it
// must NOT fail a committed write, and the durable table + the consumer's poll floor guarantee
// progress if the notification is lost — so the error is not propagated. It IS logged at warn,
// though: a persistently failing NOTIFY (e.g. a permissions issue) should be visible rather than
// silently dropped.
func (a *Adapter) notifyBackup(ctx context.Context) {
	if _, err := a.pool.Exec(ctx, "NOTIFY file_backup_outbox"); err != nil {
		a.logger.Warn("backup-outbox NOTIFY failed (best-effort; the consumer's poll floor still drains)",
			zap.Error(err))
	}
}
