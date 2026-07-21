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

-- name: DeleteBackupOutboxPendingForFile :execrows
-- Orphan hygiene: when THIS file's last-referenced blob is removed (refcount→0), its own
-- still-pending outbox hint points at bytes that no longer exist — the consumer's fetch could
-- only ever 404. file-service is the one deleting the blob, so it removes the dead hint too
-- (master-of-orphans). Scoped to ("fileId", "externalID"), NOT the hash alone: a concurrent
-- re-upload of the SAME content by ANOTHER document enqueues a row with the same "externalID" but
-- a different "fileId", which must not be touched. The NOT EXISTS guard closes the remaining
-- same-document race: if a concurrent replace put THIS document's content BACK to this hash
-- (A→B→A) and re-enqueued it, the file row again ties $1 to $2, so its still-live hint is spared —
-- the row is deleted ONLY when this document genuinely no longer references this content, checked
-- atomically against the owner's own `file` table (producer-side; not the consumer's wall).
-- in_progress rows are left to the consumer's own 404→skip backstop; done/skipped/dead_letter
-- are history, untouched.
DELETE FROM file_backup_outbox o
WHERE o."fileId" = $1 AND o."externalID" = $2 AND o.status = 'pending'
  AND NOT EXISTS (SELECT 1 FROM file f WHERE f.id = $1 AND f."externalID" = $2);
