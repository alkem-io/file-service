package model

import "errors"

// ErrDocumentNotFound is returned when a document cannot be found in the repository.
var ErrDocumentNotFound = errors.New("document not found")

// ErrDuplicateKey is returned when an insert would violate a unique constraint
// (e.g., the unique index on file(externalID, storageBucketId)).
// Service layer uses this to detect concurrent-creator races.
var ErrDuplicateKey = errors.New("duplicate key")
