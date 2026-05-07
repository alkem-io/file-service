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
UPDATE file
SET "externalID" = $2, "mimeType" = $3, size = $4, "updatedDate" = $5,
    content_metadata = $6
WHERE id = $1;

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
