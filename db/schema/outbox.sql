-- sqlc SCHEMA MIRROR of the backup outbox — codegen input ONLY (not a migration).
-- The table DDL is owned by **server** (008-continuous-file-backup, contract:
-- agents-hq/specs/008-continuous-file-backup/contracts/outbox.sql). file-service is the
-- writer (transactional INSERT with the `file` row + a periodic prune of done rows). Keep
-- this in lockstep with the server migration; the consumer's startup Probe SELECTs every
-- column, so drift fails loudly at deploy.
CREATE TABLE file_backup_outbox (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "fileId"     UUID         NOT NULL,
    "externalID" VARCHAR(128) NOT NULL,
    priority     SMALLINT     NOT NULL DEFAULT 0,
    status       VARCHAR(16)  NOT NULL DEFAULT 'pending',
    attempts     INT          NOT NULL DEFAULT 0,
    deliveries   INT          NOT NULL DEFAULT 0,
    "lastError"  TEXT,
    "createdBy"  UUID,
    "createdDate" TIMESTAMPTZ NOT NULL DEFAULT now(),
    size         BIGINT       NOT NULL DEFAULT 0,
    "claimedAt"  TIMESTAMPTZ,
    "visibleAt"  TIMESTAMPTZ
);
