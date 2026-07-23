-- name: GetDocumentByID :one
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata
FROM file
WHERE id = $1;

-- name: FindDocumentByExternalIDAndBucket :one
-- Deterministic selection: oldest row wins. Matters if historical duplicates
-- exist (pre-migration) or if two racing inserts both succeed before the
-- unique index lands in prod. "createdDate" ASC, then id ASC as tiebreaker.
SELECT id, "externalID", "mimeType", size, "displayName", "createdBy",
       "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
       "createdDate", "updatedDate", version, content_metadata
FROM file
WHERE "externalID" = $1 AND "storageBucketId" = $2
ORDER BY "createdDate" ASC, id ASC
LIMIT 1;

-- name: CreateDocument :one
INSERT INTO file (id, "externalID", "mimeType", size, "displayName", "createdBy",
                      "temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
                      "createdDate", "updatedDate", version, content_metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 1, $13)
RETURNING id;

-- name: UpdateDocumentFile :execrows
-- Updates content fields. content_metadata replaces any prior value (Replace
-- emits fresh dims via Process; the old row's content_metadata is discarded).
-- Compare-and-set on both the externalID and version the caller read before
-- streaming, and bump version on success. This serializes content replacement
-- with metadata updates (especially temporary→permanent promotion): neither may
-- commit based on stale routing state and thereby miss or misroute an outbox hint.
UPDATE file
SET "externalID" = $2, "mimeType" = $3, size = $4, "updatedDate" = $5,
    content_metadata = $6, version = version + 1
WHERE id = $1 AND "externalID" = $7 AND version = $8;

-- name: UpdateDocumentMetadata :execrows
-- Updates the mutable metadata fields (storageBucketId, temporaryLocation,
-- displayName) atomically with optimistic locking. Caller fills unchanged
-- fields with their current values. mimeType, externalID, size are not
-- mutable through this query — they change only via UpdateDocumentFile.
UPDATE file
SET "storageBucketId"    = $2,
    "temporaryLocation"  = $3,
    "displayName"        = $4,
    "updatedDate"        = $5,
    version              = version + 1
WHERE id = $1 AND version = $6;

-- name: BackfillContentMetadata :execrows
-- Compare-and-set: only writes when content_metadata is still empty AND
-- the row's externalID is the one we measured against. Protects against
-- the dimension sweep overwriting freshly-replaced content's metadata.
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

-- name: ListImagesNeedingDims :many
-- Paged scan for the image-dimension backfill sweep (the sweep-dims job, spec 019/020): image rows whose
-- content_metadata is still unpopulated ('{}'). Keyset-paged by id ($1 = cursor, $2 = page size) so
-- a large first-run legacy set never loads whole. The sweep reads each blob by externalID, does a
-- header-only measure, and compare-and-sets the dims via BackfillContentMetadata.
-- `file.id` is the primary key, so its B-tree index supports both `id > cursor` and ORDER BY id;
-- each row is visited at most once during this finite manual sweep. A separate schema migration
-- and partial index would add steady-state write cost for an operation intended to converge away.
SELECT id, "externalID", "mimeType"
FROM file
WHERE "mimeType" LIKE 'image/%'
  AND content_metadata = '{}'::jsonb
  AND id > $1
ORDER BY id
LIMIT $2;
