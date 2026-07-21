-- file-service is the outbox DML owner (008-continuous-file-backup). The INSERT runs INSIDE
-- the same transaction as the `file` row (FR-001: write-then-record; no committed outbox
-- entry without a file row). The consumer (file-backup-service) reads/claims via a scoped role.

-- name: EnqueueBackupOutbox :exec
-- Record a content-hash backup hint for a newly stored/replaced object.
INSERT INTO file_backup_outbox ("fileId", "externalID", priority, "createdBy", "createdDate", size)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: PruneBackupOutboxDone :execrows
-- Keep the outbox bounded (SC-008): drop rows the consumer already finished (status='done')
-- older than a retention cutoff. The ledger (file-backup-service's own DB) keeps the durable
-- record. Only 'done' is pruned — pending/in_progress/dead_letter/skipped are retained.
DELETE FROM file_backup_outbox
WHERE status = 'done' AND "createdDate" < $1;

-- name: DeleteBackupOutboxPendingByHash :execrows
-- Orphan hygiene: when the LAST file row referencing a blob is deleted and the blob itself is
-- removed (refcount→0), any still-pending outbox row for that hash points at bytes that no
-- longer exist — the consumer's fetch could only ever 404. file-service is the one deleting the
-- blob, so it removes the dead hint too (master-of-orphans). in_progress rows are left to the
-- consumer's own 404→skip backstop; done/skipped/dead_letter are history, untouched.
DELETE FROM file_backup_outbox
WHERE "externalID" = $1 AND status = 'pending';
