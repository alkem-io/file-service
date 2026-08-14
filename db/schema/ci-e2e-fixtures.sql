-- Minimal tables the end-to-end sweep test writes to, for CI only.
--
-- NOT a schema definition. `file` is mirrored in document.sql for sqlc codegen; these
-- two are server-owned and appear here solely because the e2e fixture inserts rows to
-- satisfy the shape the real database has. Only the columns the test actually writes
-- are declared, so this cannot drift into a second source of truth for either table —
-- if the real schema gains a NOT NULL column the test does not set, the test fails
-- against a real database, which is the environment that matters.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS authorization_policy (
    id UUID PRIMARY KEY,
    "credentialRules" TEXT NOT NULL,
    "privilegeRules" TEXT NOT NULL,
    type TEXT NOT NULL,
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS storage_bucket (
    id UUID PRIMARY KEY,
    "createdDate" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedDate" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version INTEGER NOT NULL,
    "allowedMimeTypes" TEXT NOT NULL,
    "maxFileSize" INTEGER NOT NULL,
    "authorizationId" UUID,
    "storageAggregatorId" UUID
);
