-- name: GetDocumentByID :one
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata, "externalReference"
FROM file
WHERE id = $1;

-- name: FindDocumentByExternalIDAndBucket :one
-- Content-dedup lookup for PLAIN (non-reference) uploads only. The
-- "externalReference" IS NULL filter is the keystone of the dual-identity
-- rule: reference-bearing rows are identity'd by their externalReference, so
-- a plain upload must never dedup onto (and thus couple its lifecycle to) a
-- reference-bearing row that happens to share the same bytes. Plain rows
-- dedup among plain rows; reference rows are resolved by reference.
-- Deterministic selection: oldest row wins. Matters if historical duplicates
-- exist (pre-migration) or if two racing inserts both succeed (prod has no
-- externalID unique index). "createdDate" ASC, then id ASC as tiebreaker.
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata, "externalReference"
FROM file
WHERE "externalID" = $1 AND "storageBucketId" = $2 AND "externalReference" IS NULL
ORDER BY "createdDate" ASC, id ASC
LIMIT 1;

-- name: GetDocumentByReference :one
-- Global by-reference lookup (provider fetch): resolves an opaque
-- externalReference across ALL buckets. Several buckets may carry the same
-- reference (re-share, one shared blob); oldest row wins deterministically.
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata, "externalReference"
FROM file
WHERE "externalReference" = $1
ORDER BY "createdDate" ASC, id ASC
LIMIT 1;

-- name: GetDocumentByReferenceInBucket :one
-- Bucket-scoped by-reference lookup (read resolution): the single document
-- carrying this reference in the given bucket. The partial
-- UNIQUE(externalReference, storageBucketId) normally guarantees at most one
-- match; ORDER BY "createdDate" ASC, id ASC keeps resolution deterministic
-- (oldest wins) as defense-in-depth, matching the global by-reference query,
-- even if that constraint were ever absent.
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata, "externalReference"
FROM file
WHERE "externalReference" = $1 AND "storageBucketId" = $2
ORDER BY "createdDate" ASC, id ASC
LIMIT 1;

-- name: CreateDocument :one
INSERT INTO file (id, "externalID", "mimeType", size, "displayName", "createdBy",
                      "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
                      "createdDate", "updatedDate", version, content_metadata, "externalReference")
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 1, $13, $14)
RETURNING id;

-- name: UpdateDocumentFile :execrows
-- Updates content fields. content_metadata replaces any prior value (Replace
-- emits fresh dims via Process; the old row's content_metadata is discarded).
UPDATE file
SET "externalID" = $2, "mimeType" = $3, size = $4, "updatedDate" = $5,
    content_metadata = $6
WHERE id = $1;

-- name: UpdateDocumentMetadata :one
-- Updates the mutable metadata fields atomically with optimistic locking.
-- Caller fills unchanged fields with their current values. This is the
-- "move + re-attribute" primitive: besides storageBucketId/temporaryLocation/
-- displayName it also re-points authorizationId, createdBy, and the opaque
-- externalReference. mimeType, externalID, size are not mutable here — they
-- change only via UpdateDocumentFile.
--
-- RETURNING "externalID", size yields the AUTHORITATIVE post-update content
-- identity read from the row while it is UPDATE-locked in this transaction.
-- The transactional backup-outbox producer enqueues that value, not a
-- handler-threaded snapshot, so a concurrent content-replace (which swaps
-- externalID/size WITHOUT bumping version) can't leave the outbox pointing at
-- a stale hash — it either committed before this UPDATE (RETURNING sees the
-- new hash) or blocks on the row lock until this tx commits (then replaces on
-- a now-durable row and enqueues its own outbox). 0 rows (version mismatch /
-- missing row) → no row returned (pgx.ErrNoRows), which the adapter maps to
-- model.ErrDocumentNotFound.
UPDATE file
SET "storageBucketId"    = $2,
    "temporaryLocation"  = $3,
    "displayName"        = $4,
    "authorizationId"    = $5,
    "createdBy"          = $6,
    "externalReference"  = $7,
    "updatedDate"        = $8,
    version              = version + 1
WHERE id = $1 AND version = $9
RETURNING "externalID", size;

-- name: BackfillContentMetadata :execrows
-- Compare-and-set: only writes when content_metadata is still empty AND
-- the row's externalID is the one we measured against. Protects against
-- the lazy-backfill overwriting freshly-replaced content's metadata.
-- Updates only content_metadata; does NOT bump version (FR-018).
UPDATE file
SET content_metadata = $2
WHERE id = $1
  AND "externalID" = $3
  AND content_metadata = '{}'::jsonb;

-- name: DeleteDocument :one
DELETE FROM file
WHERE id = $1
RETURNING "externalID", "authorizationId", "tagsetId";

-- name: CountDocumentsByExternalID :one
SELECT COUNT(*) FROM file WHERE "externalID" = $1;

-- name: ListDocumentsByMimeTypes :many
-- Repair-job scan (spec 019): rows whose stored type is one of the given
-- (generic) MIME types. Office-extension filtering happens in the domain
-- layer, which owns the office vocabulary.
SELECT id, "externalID", "mimeType", size, "displayName"
FROM file
WHERE "mimeType" = ANY($1::text[])
ORDER BY "createdDate" ASC;

-- name: UpdateDocumentMimeType :execrows
-- Repair-job relabel (spec 019): corrects only the stored MIME type.
-- Deliberately narrow — content fields change exclusively via
-- UpdateDocumentFile. Compare-and-set on externalID: the relabel applies
-- only if the content is still the one the repair job sniffed, protecting
-- against a concurrent Replace (which already set the correct MIME type)
-- landing between the suspect scan and this write.
UPDATE file
SET "mimeType"    = $2,
    "updatedDate" = $3,
    version       = version + 1
WHERE id = $1
  AND "externalID" = $4;
