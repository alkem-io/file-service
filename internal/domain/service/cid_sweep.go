package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// CIDSweeper performs the removable, sequential legacy-CID migration. Each
// source is independent: a group failure is recorded and later groups are
// still attempted.
type CIDSweeper struct {
	Repo    port.CIDMigrationRepo
	Storage port.CIDMigrationStorage
}

// Run migrates every present CID candidate returned by the repository.
func (s *CIDSweeper) Run(ctx context.Context) (port.CIDSweepResult, error) {
	result := port.CIDSweepResult{}
	if s.Repo == nil || s.Storage == nil {
		result.Aborted = true
		return result, errors.New("CID sweeper requires repository and storage")
	}

	candidates, err := s.Repo.ListCIDCandidates(ctx)
	if err != nil {
		result.Aborted = true
		return result, fmt.Errorf("list CID candidates: %w", err)
	}
	if len(candidates) == 0 {
		return result, nil
	}

	caseAliases, err := s.Repo.ListCIDCaseAliases(ctx)
	if err != nil {
		result.Aborted = true
		return result, fmt.Errorf("list CID case aliases: %w", err)
	}
	aliasesByTarget := groupCIDCaseAliases(caseAliases)
	countedCaseTargets := make(map[string]struct{}, len(aliasesByTarget))

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Aborted = true
			return result, err
		}
		distinctBefore := result.DistinctCIDSources
		if err := s.migrateCandidate(ctx, candidate, aliasesByTarget, countedCaseTargets, &result); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				if result.DistinctCIDSources > distinctBefore {
					recordCIDSweepFailure(&result, candidate.ExternalID, err)
				}
				result.Aborted = true
				return result, ctxErr
			}
			recordCIDSweepFailure(&result, candidate.ExternalID, err)
		}
	}
	return result, nil
}

func recordCIDSweepFailure(result *port.CIDSweepResult, cid string, err error) {
	result.FailedSourceBlobs++
	result.Failures = append(result.Failures, port.CIDSweepFailure{
		CID:    cid,
		Reason: err.Error(),
	})
}

func (s *CIDSweeper) migrateCandidate(
	ctx context.Context,
	candidate port.CIDCandidate,
	aliasesByTarget map[string][]port.CIDAlias,
	countedCaseTargets map[string]struct{},
	result *port.CIDSweepResult,
) error {
	reader, err := s.Storage.OpenCIDSource(ctx, candidate.ExternalID)
	if err != nil {
		// Presence is part of eligibility. A stale database reference with no
		// corresponding source is deliberately outside this incident sweep.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		result.DistinctCIDSources++
		result.CIDReferencesFound += candidate.ReferenceCount
		return fmt.Errorf("open source: %w", err)
	}

	result.DistinctCIDSources++
	result.CIDReferencesFound += candidate.ReferenceCount
	target, err := hashCIDSource(ctx, reader)
	if err != nil {
		return err
	}

	aliases := aliasesByTarget[target]
	exactAliases, caseVariantReferences := exactUpdateAliases(candidate.ExternalID, target, aliases)
	if _, counted := countedCaseTargets[target]; !counted {
		result.CaseVariantReferencesFound += caseVariantReferences
		countedCaseTargets[target] = struct{}{}
	}

	prepared, err := s.Storage.PrepareCIDTarget(ctx, candidate.ExternalID, target)
	if err != nil {
		return fmt.Errorf("prepare lowercase target: %w", err)
	}

	updated, err := s.Repo.UpdateCIDGroup(ctx, target, exactAliases)
	if err != nil {
		return fmt.Errorf("update references: %w", err)
	}
	result.ReferencesUpdated += updated

	if err := s.finalizeAliases(ctx, prepared.ObsoleteAliases, target); err != nil {
		return err
	}

	if prepared.Created {
		result.MigratedSourceBlobs++
	} else {
		result.ConsolidatedSourceBlobs++
	}
	return nil
}

func groupCIDCaseAliases(aliases []port.CIDAlias) map[string][]port.CIDAlias {
	result := make(map[string][]port.CIDAlias)
	for _, alias := range aliases {
		target := strings.ToLower(alias.ExternalID)
		result[target] = append(result[target], alias)
	}
	return result
}

func exactUpdateAliases(source, target string, aliases []port.CIDAlias) ([]string, int64) {
	exactAliases := []string{source}
	seen := map[string]struct{}{source: {}}
	var caseVariantReferences int64
	for _, alias := range aliases {
		if alias.ExternalID == target {
			continue
		}
		caseVariantReferences += alias.ReferenceCount
		if _, exists := seen[alias.ExternalID]; exists {
			continue
		}
		exactAliases = append(exactAliases, alias.ExternalID)
		seen[alias.ExternalID] = struct{}{}
	}
	return exactAliases, caseVariantReferences
}

func (s *CIDSweeper) finalizeAliases(ctx context.Context, aliases []port.CIDStorageAlias, target string) error {
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		if _, exists := seen[alias.Name]; exists {
			continue
		}
		seen[alias.Name] = struct{}{}
		remaining, err := s.Repo.CountCIDAliasReferences(ctx, alias.Name)
		if err != nil {
			return fmt.Errorf("count references for obsolete alias %s: %w", alias.Name, err)
		}
		if remaining != 0 {
			return fmt.Errorf("obsolete alias %s still has %d references", alias.Name, remaining)
		}
		if err := s.Storage.FinalizeCIDAlias(ctx, alias, target); err != nil {
			return fmt.Errorf("finalize obsolete alias %s: %w", alias.Name, err)
		}
	}
	return nil
}

func hashCIDSource(ctx context.Context, reader io.ReadCloser) (string, error) {
	target, err := HashReadCloser(ctx, reader)
	if err != nil {
		return "", fmt.Errorf("hash source: %w", err)
	}
	return target, nil
}
