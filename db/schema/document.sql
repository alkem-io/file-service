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
-- while still sharing one content-addressed blob. The pre-existing prod
-- UNIQUE("externalID", "storageBucketId") (owned by the server's TypeORM
-- migration) must be made partial in the same way.
CREATE UNIQUE INDEX "UQ_file_externalID_storageBucketId"
    ON file ("externalID", "storageBucketId")
    WHERE "externalReference" IS NULL;

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
