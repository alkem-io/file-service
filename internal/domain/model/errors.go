package model

import "errors"

// ErrDocumentNotFound is returned when a document cannot be found in the repository.
var ErrDocumentNotFound = errors.New("document not found")

// ErrDuplicateKey is the sentinel every unique-constraint violation matches
// under errors.Is. Adapters return the richer *DuplicateKeyError below, which
// additionally identifies WHICH index was violated; a caller that only needs
// "was this a duplicate?" keeps matching this sentinel.
var ErrDuplicateKey = errors.New("duplicate key")

// UniqueConstraint identifies which unique index a write collided on.
//
// This exists because the correct resolution DIFFERS per index — a collision on
// (externalReference, storageBucketId) is an idempotent re-share and resolves to
// the existing row; a collision on any other index is a genuine conflict — and
// PROBING for the answer is unsound. Re-querying to guess cannot distinguish
// "the violation came from another index" from "the winner was deleted a
// microsecond later", and a probe that hits on the wrong index returns an
// unrelated row as if it were the winner. Postgres names the violated constraint
// on every unique violation, so the adapter classifies that name into this enum
// and the domain branches on knowledge instead of a guess.
type UniqueConstraint int

const (
	// ConstraintUnspecified means the violation carried NO constraint name, so
	// which index was violated is not known. Callers must fail loudly rather
	// than pick a resolution at random.
	ConstraintUnspecified UniqueConstraint = iota
	// ConstraintExternalReferenceBucket is the partial
	// UNIQUE(externalReference, storageBucketId) index: this (reference, bucket)
	// pair is already materialized.
	ConstraintExternalReferenceBucket
	// ConstraintOther is any other unique index on the table — authorizationId,
	// tagsetId, the primary key, or a content index where one exists.
	ConstraintOther
)

// DuplicateKeyError is a unique-constraint violation together with the identity
// of the index that raised it. It matches ErrDuplicateKey under errors.Is, so
// existing "is this a duplicate?" checks are unaffected; a caller that must
// RESOLVE the collision reads Constraint via errors.As.
type DuplicateKeyError struct {
	// Constraint is the classified index, the only field callers branch on.
	Constraint UniqueConstraint
	// Name is the raw index name the database reported, carried for diagnostics
	// (logs, error text) only. Classification happens once, in the adapter that
	// knows the schema; nothing outside it may branch on this string.
	Name string
}

func (e *DuplicateKeyError) Error() string {
	if e.Name == "" {
		return "duplicate key: the database did not name the violated constraint"
	}
	return "duplicate key on " + e.Name
}

// Is makes every DuplicateKeyError match the ErrDuplicateKey sentinel, so
// callers can keep branching with errors.Is and only reach for errors.As when
// they need the constraint identity.
func (e *DuplicateKeyError) Is(target error) bool { return target == ErrDuplicateKey }
