package port

import (
	"context"
	"io"
)

// CIDCandidate is one distinct incident CIDv0 recorded in the file table.
type CIDCandidate struct {
	ExternalID     string
	ReferenceCount int64
}

// CIDAlias is one database spelling of a SHA3 target and its global refcount.
type CIDAlias struct {
	ExternalID     string
	ReferenceCount int64
}

// CIDStorageAlias is an obsolete physical name discovered for one migration
// group. CanonicalizeCase is true only when a case-insensitive volume exposes a
// non-lowercase SHA3 directory entry through the lowercase path.
type CIDStorageAlias struct {
	Name             string
	CanonicalizeCase bool
}

// CIDTargetPreparation describes the verified storage state before references
// move. Created distinguishes a newly published target from consolidation onto
// a target or case-variant that already existed.
type CIDTargetPreparation struct {
	Created         bool
	ObsoleteAliases []CIDStorageAlias
}

// CIDSweepFailure is one independently failed CID group.
type CIDSweepFailure struct {
	CID    string `json:"cid"`
	Reason string `json:"reason"`
}

// CIDSweepResult is the complete terminal accounting emitted by the command.
type CIDSweepResult struct {
	CIDReferencesFound         int64             `json:"cid_references_found"`
	CaseVariantReferencesFound int64             `json:"case_variant_references_found"`
	ReferencesUpdated          int64             `json:"references_updated"`
	DistinctCIDSources         int64             `json:"distinct_cid_sources"`
	MigratedSourceBlobs        int64             `json:"migrated_source_blobs"`
	ConsolidatedSourceBlobs    int64             `json:"consolidated_source_blobs"`
	FailedSourceBlobs          int64             `json:"failed_source_blobs"`
	Aborted                    bool              `json:"aborted"`
	Failures                   []CIDSweepFailure `json:"failures,omitempty"`
}

// Complete reports whether every discovered present CID source completed.
func (r CIDSweepResult) Complete() bool {
	return !r.Aborted && r.FailedSourceBlobs == 0
}

// CIDMigrationRepo is the one-off database surface. It deliberately changes
// only file.externalID and does not expand the ordinary DocumentRepo.
type CIDMigrationRepo interface {
	// ListCIDCandidates returns distinct referenced incident CIDv0 spellings.
	ListCIDCandidates(ctx context.Context) ([]CIDCandidate, error)
	// ListCIDCaseAliases returns every referenced non-lowercase SHA3 spelling.
	// The sweep loads this small legacy set once and groups it by lowercase
	// target, avoiding one full-table expression scan per CID source.
	ListCIDCaseAliases(ctx context.Context) ([]CIDAlias, error)
	// UpdateCIDGroup atomically repoints every exact obsolete alias.
	UpdateCIDGroup(ctx context.Context, lowercaseTarget string, exactAliases []string) (int64, error)
	// CountCIDAliasReferences returns the global exact-spelling refcount.
	CountCIDAliasReferences(ctx context.Context, exactAlias string) (int64, error)
}

// CIDMigrationStorage is the one-off local-storage surface. The service calls
// FinalizeCIDAlias only after the repository reports zero exact references.
type CIDMigrationStorage interface {
	// OpenCIDSource opens the exact recorded CID spelling for streamed hashing.
	OpenCIDSource(ctx context.Context, externalID string) (io.ReadCloser, error)
	// PrepareCIDTarget verifies aliases and makes the lowercase path readable.
	PrepareCIDTarget(ctx context.Context, sourceExternalID, lowercaseTarget string) (CIDTargetPreparation, error)
	// FinalizeCIDAlias parks one alias after its exact refcount reaches zero.
	FinalizeCIDAlias(ctx context.Context, alias CIDStorageAlias, lowercaseTarget string) error
}
