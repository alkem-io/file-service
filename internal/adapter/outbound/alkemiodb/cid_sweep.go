package alkemiodb

import (
	"context"

	"github.com/alkem-io/file-service/internal/adapter/outbound/alkemiodb/queries"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// ListCIDCandidates returns the exact incident CIDv0-shaped database cohort.
// Storage presence is checked by the service before a candidate is counted as
// eligible.
func (a *Adapter) ListCIDCandidates(ctx context.Context) ([]port.CIDCandidate, error) {
	rows, err := a.queries.ListCIDCandidates(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]port.CIDCandidate, len(rows))
	for i, row := range rows {
		result[i] = port.CIDCandidate{
			ExternalID:     row.ExternalID,
			ReferenceCount: row.ReferenceCount,
		}
	}
	return result, nil
}

// ListCIDCaseAliases returns all referenced non-lowercase SHA3 spellings in
// one query. The service groups this legacy set by lowercase target in memory.
func (a *Adapter) ListCIDCaseAliases(ctx context.Context) ([]port.CIDAlias, error) {
	rows, err := a.queries.ListCIDCaseAliases(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]port.CIDAlias, len(rows))
	for i, row := range rows {
		result[i] = port.CIDAlias{
			ExternalID:     row.ExternalID,
			ReferenceCount: row.ReferenceCount,
		}
	}
	return result, nil
}

// UpdateCIDGroup atomically changes only externalID for every exact obsolete
// alias in one set-based statement.
func (a *Adapter) UpdateCIDGroup(ctx context.Context, lowercaseTarget string, exactAliases []string) (int64, error) {
	return a.queries.UpdateCIDGroup(ctx, queries.UpdateCIDGroupParams{
		LowercaseTarget: lowercaseTarget,
		ExactAliases:    exactAliases,
	})
}

// CountCIDAliasReferences returns the global exact-spelling reference count
// that gates parking and physical case canonicalization.
func (a *Adapter) CountCIDAliasReferences(ctx context.Context, exactAlias string) (int64, error) {
	return a.queries.CountCIDAliasReferences(ctx, exactAlias)
}

var _ port.CIDMigrationRepo = (*Adapter)(nil)
