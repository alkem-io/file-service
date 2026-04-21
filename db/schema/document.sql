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
    "temporaryLocation" BOOLEAN NOT NULL DEFAULT FALSE,
    "authorizationId" UUID UNIQUE,
    "storageBucketId" UUID,
    "tagsetId" UUID UNIQUE
);
