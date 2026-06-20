package model

import "errors"

// ErrDocumentNotFound is returned when a document cannot be found in the repository.
var ErrDocumentNotFound = errors.New("document not found")

// ErrDuplicateKey is returned when an insert violates a UNIQUE constraint on
// the file table — the per-bucket content-dedup index on (externalID,
// storageBucketId) where the schema defines it, OR the UNIQUE(authorizationId)
// / UNIQUE(tagsetId) columns. The adapter cannot tell these apart portably
// (TypeORM names them with opaque REL_* hashes), so the service layer
// disambiguates by re-querying the (externalID, bucket) dedup scope: a hit is
// the content race; a miss means an authorizationId/tagsetId collision.
var ErrDuplicateKey = errors.New("duplicate key")
