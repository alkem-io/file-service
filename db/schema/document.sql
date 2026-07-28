-- ⚠️ SCHEMA OWNERSHIP: this file is the **sqlc codegen source** only — a MIRROR of
-- the prod schema. The prod `file` table is created and MIGRATED by the **server's
-- TypeORM migrations** (run by the `server-migration` cron via `pnpm migration:run`);
-- file-service does full runtime CRUD on the table but does NOT migrate it.
-- Any schema change here MUST have a matching server TypeORM migration, or it will
-- exist locally/in tests but be absent in prod. (There is intentionally no runnable
-- `db/migrations/` here — it would be misleading.)
CREATE TABLE file (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    "createdDate" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedDate" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version INTEGER NOT NULL DEFAULT 1,
    "createdBy" UUID,
    "displayName" VARCHAR(512) NOT NULL DEFAULT '',
    "mimeType" VARCHAR(128) NOT NULL,
    size INTEGER NOT NULL DEFAULT 0,
    "externalID" VARCHAR(128) NOT NULL,
    "externalReference" VARCHAR(256),
    "temporaryLocation" BOOLEAN NOT NULL DEFAULT FALSE,
    "authorizationId" UUID UNIQUE,
    "storageBucketId" UUID,
    "tagsetId" UUID UNIQUE,
    content_metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- Content-dedup uniqueness is PARTIAL: it applies only to rows WITHOUT an
-- externalReference. Reference-bearing rows are identity'd by their reference
-- (the UQ below), not by content, so two distinct references with identical
-- bytes coexist in one bucket — each separately by-reference-resolvable —
-- while still sharing one content-addressed blob.
--
-- NOTE: prod has NO UNIQUE("externalID","storageBucketId") constraint at all
-- (the server TypeORM baseline never created one; content-dedup is app-level
-- via FindByExternalIDAndBucket). So this schema mirror also omits it — adding
-- one would diverge from prod and could false-green the content-dedup race in
-- local tests. Do NOT add an externalID unique here without a matching, safe
-- server TypeORM migration (which would first have to de-dup legacy rows).

-- externalReference is an OPAQUE caller-supplied reference (e.g. a Synapse
-- media_id for Matrix media); file-service never parses it. Distinct from
-- "externalID" (the content hash / blob key). At most one row may carry a
-- given reference within a single bucket; the same reference may appear in
-- several buckets when media is re-shared (each row sharing one blob).
CREATE UNIQUE INDEX "UQ_file_externalReference_storageBucketId"
    ON file ("externalReference", "storageBucketId")
    WHERE "externalReference" IS NOT NULL;

-- Supporting index for the global by-reference lookup (provider fetch), which
-- resolves a reference across all buckets.
CREATE INDEX "IDX_file_externalReference"
    ON file ("externalReference")
    WHERE "externalReference" IS NOT NULL;
