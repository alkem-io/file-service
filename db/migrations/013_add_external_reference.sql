-- 013-matrix-media-file-service
--
-- Adds an opaque, indexed "externalReference" to the file table so callers
-- (the Synapse media storage provider) can address a document by an external
-- key (the Matrix media_id) without file-service ever parsing it. Distinct
-- from "externalID" (the SHA3-256 content hash / blob key).
--
-- Additive and backward-compatible: the column is nullable, so every existing
-- row and every existing caller is unaffected.
--
-- NOTE: file-service does not run its own migrations — the Alkemio server owns
-- the shared `file` table's TypeORM migrations. This file is the canonical DDL
-- for the change and the source of truth for db/schema/document.sql (sqlc).

ALTER TABLE file ADD COLUMN IF NOT EXISTS "externalReference" VARCHAR(256);

-- At most one row per (reference, bucket); the same reference may recur across
-- buckets (re-share, shared blob). Partial so NULLs never collide.
CREATE UNIQUE INDEX IF NOT EXISTS "UQ_file_externalReference_storageBucketId"
    ON file ("externalReference", "storageBucketId")
    WHERE "externalReference" IS NOT NULL;

-- Supports the global by-reference lookup (provider fetch across all buckets).
CREATE INDEX IF NOT EXISTS "IDX_file_externalReference"
    ON file ("externalReference")
    WHERE "externalReference" IS NOT NULL;
