package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

const cidSweepParkingDir = "_parked/ipfs-cid-sweep"

type fileOps interface {
	open(name string) (io.ReadCloser, error)
	stat(name string) (os.FileInfo, error)
	readDir(name string) ([]os.DirEntry, error)
	mkdirAll(path string, perm os.FileMode) error
	link(oldname, newname string) error
	rename(oldpath, newpath string) error
}

type osFileOps struct{}

func (osFileOps) open(name string) (io.ReadCloser, error) {
	return os.Open(name) //nolint:gosec // caller validates the flat storage key
}
func (osFileOps) stat(name string) (os.FileInfo, error)        { return os.Stat(name) }
func (osFileOps) readDir(name string) ([]os.DirEntry, error)   { return os.ReadDir(name) }
func (osFileOps) mkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }
func (osFileOps) link(oldname, newname string) error           { return os.Link(oldname, newname) }
func (osFileOps) rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }

// CIDMigrationAdapter implements only the migration storage port. Ordinary
// serving reads and writes continue to use Adapter.
type CIDMigrationAdapter struct {
	basePath            string
	ops                 fileOps
	storageIndexRead    bool
	activeEntries       map[string]struct{}
	targetVariantsByKey map[string][]string
}

// NewCIDMigration creates the removable migration adapter rooted at basePath.
func NewCIDMigration(basePath string) *CIDMigrationAdapter {
	return newCIDMigration(basePath, osFileOps{})
}

func newCIDMigration(basePath string, ops fileOps) *CIDMigrationAdapter {
	return &CIDMigrationAdapter{basePath: basePath, ops: ops}
}

// OpenCIDSource opens the recorded source spelling without normalizing it.
func (a *CIDMigrationAdapter) OpenCIDSource(ctx context.Context, externalID string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !isValidExternalID(externalID) {
		return nil, fmt.Errorf("open CID source %q: %w", externalID, port.ErrInvalidKey)
	}
	if err := a.loadStorageIndex(ctx); err != nil {
		return nil, err
	}
	if _, exists := a.activeEntries[externalID]; !exists {
		return nil, fmt.Errorf("open CID source %s: exact directory entry is absent: %w", externalID, fs.ErrNotExist)
	}
	reader, err := a.ops.open(filepath.Join(a.basePath, externalID))
	if err != nil {
		return nil, fmt.Errorf("open CID source %s: %w", externalID, err)
	}
	return reader, nil
}

// PrepareCIDTarget verifies every physical target spelling and makes the
// lowercase target path readable before the database update. On a
// case-insensitive volume it deliberately leaves the actual non-lowercase
// directory spelling untouched until FinalizeCIDAlias is called after zero
// exact references.
func (a *CIDMigrationAdapter) PrepareCIDTarget(ctx context.Context, sourceExternalID, lowercaseTarget string) (port.CIDTargetPreparation, error) {
	if err := a.validatePreparation(ctx, sourceExternalID, lowercaseTarget); err != nil {
		return port.CIDTargetPreparation{}, err
	}
	variants, hasExactLowercase, err := a.targetVariants(ctx, lowercaseTarget)
	if err != nil {
		return port.CIDTargetPreparation{}, err
	}
	caseFoldVariant, err := a.caseFoldVariant(lowercaseTarget, variants, hasExactLowercase)
	if err != nil {
		return port.CIDTargetPreparation{}, err
	}

	result := port.CIDTargetPreparation{
		ObsoleteAliases: []port.CIDStorageAlias{{Name: sourceExternalID}},
	}
	if caseFoldVariant != "" {
		// The lowercase path already resolves the verified variant. Do not
		// create parking or change its physical spelling before the DB update.
		if err := a.verifyDigest(ctx, lowercaseTarget, lowercaseTarget); err != nil {
			return port.CIDTargetPreparation{}, fmt.Errorf("verify lowercase target path: %w", err)
		}
	} else if !hasExactLowercase {
		created, err := a.publishLowercaseTarget(ctx, sourceExternalID, lowercaseTarget)
		if err != nil {
			return port.CIDTargetPreparation{}, err
		}
		// A pre-existing case variant means this source consolidates even when
		// a separate lowercase hard link had to be published on a
		// case-sensitive volume.
		result.Created = created && len(variants) == 0
	}

	for _, variant := range variants {
		if variant == lowercaseTarget {
			continue
		}
		result.ObsoleteAliases = append(result.ObsoleteAliases, port.CIDStorageAlias{
			Name:             variant,
			CanonicalizeCase: variant == caseFoldVariant,
		})
	}
	return result, nil
}

func (a *CIDMigrationAdapter) validatePreparation(ctx context.Context, sourceExternalID, lowercaseTarget string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isValidExternalID(sourceExternalID) {
		return fmt.Errorf("prepare source %q: %w", sourceExternalID, port.ErrInvalidKey)
	}
	if !isLowerSHA3(lowercaseTarget) {
		return fmt.Errorf("prepare target %q: %w", lowercaseTarget, port.ErrInvalidKey)
	}
	if err := a.loadStorageIndex(ctx); err != nil {
		return err
	}
	if _, exists := a.activeEntries[sourceExternalID]; !exists {
		return fmt.Errorf("prepare CID source %s: exact directory entry is absent: %w", sourceExternalID, fs.ErrNotExist)
	}
	info, err := a.ops.stat(filepath.Join(a.basePath, sourceExternalID))
	if err != nil {
		return fmt.Errorf("stat CID source %s: %w", sourceExternalID, err)
	}
	if info.IsDir() {
		return fmt.Errorf("CID source %s is a directory", sourceExternalID)
	}
	return nil
}

func (a *CIDMigrationAdapter) targetVariants(ctx context.Context, lowercaseTarget string) ([]string, bool, error) {
	if err := a.loadStorageIndex(ctx); err != nil {
		return nil, false, err
	}
	variants := append([]string(nil), a.targetVariantsByKey[lowercaseTarget]...)
	hasExactLowercase := false
	for _, variant := range variants {
		if err := a.verifyDigest(ctx, variant, lowercaseTarget); err != nil {
			return nil, false, err
		}
		hasExactLowercase = hasExactLowercase || variant == lowercaseTarget
	}
	return variants, hasExactLowercase, nil
}

func (a *CIDMigrationAdapter) loadStorageIndex(ctx context.Context) error {
	if a.storageIndexRead {
		return nil
	}
	entries, err := a.ops.readDir(a.basePath)
	if err != nil {
		return fmt.Errorf("list storage root: %w", err)
	}
	activeEntries := make(map[string]struct{}, len(entries))
	variantsByKey := make(map[string][]string)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			continue
		}
		activeEntries[entry.Name()] = struct{}{}
		if !isSHA3(entry.Name()) {
			continue
		}
		key := strings.ToLower(entry.Name())
		variantsByKey[key] = append(variantsByKey[key], entry.Name())
	}
	a.activeEntries = activeEntries
	a.targetVariantsByKey = variantsByKey
	a.storageIndexRead = true
	return nil
}

func (a *CIDMigrationAdapter) addTargetVariant(name string) {
	if !a.storageIndexRead {
		return
	}
	a.activeEntries[name] = struct{}{}
	if !isSHA3(name) {
		return
	}
	key := strings.ToLower(name)
	for _, existing := range a.targetVariantsByKey[key] {
		if existing == name {
			return
		}
	}
	a.targetVariantsByKey[key] = append(a.targetVariantsByKey[key], name)
}

func (a *CIDMigrationAdapter) removeTargetVariant(name string) {
	if !a.storageIndexRead {
		return
	}
	delete(a.activeEntries, name)
	if !isSHA3(name) {
		return
	}
	key := strings.ToLower(name)
	variants := a.targetVariantsByKey[key]
	for i, existing := range variants {
		if existing != name {
			continue
		}
		variants = append(variants[:i], variants[i+1:]...)
		if len(variants) == 0 {
			delete(a.targetVariantsByKey, key)
		} else {
			a.targetVariantsByKey[key] = variants
		}
		return
	}
}

func (a *CIDMigrationAdapter) caseFoldVariant(lowercaseTarget string, variants []string, hasExactLowercase bool) (string, error) {
	if hasExactLowercase || len(variants) == 0 {
		return "", nil
	}
	lowerInfo, err := a.ops.stat(filepath.Join(a.basePath, lowercaseTarget))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat lowercase target %s: %w", lowercaseTarget, err)
	}
	for _, variant := range variants {
		variantInfo, err := a.ops.stat(filepath.Join(a.basePath, variant))
		if err != nil {
			return "", fmt.Errorf("stat target alias %s: %w", variant, err)
		}
		if os.SameFile(lowerInfo, variantInfo) {
			return variant, nil
		}
	}
	return "", nil
}

func (a *CIDMigrationAdapter) publishLowercaseTarget(ctx context.Context, sourceExternalID, lowercaseTarget string) (bool, error) {
	sourcePath := filepath.Join(a.basePath, sourceExternalID)
	targetPath := filepath.Join(a.basePath, lowercaseTarget)
	if err := a.ops.link(sourcePath, targetPath); err != nil {
		if !os.IsExist(err) {
			return false, fmt.Errorf("publish lowercase target %s: %w", lowercaseTarget, err)
		}
		if err := a.verifyDigest(ctx, lowercaseTarget, lowercaseTarget); err != nil {
			return false, err
		}
		a.addTargetVariant(lowercaseTarget)
		return false, nil
	}
	a.addTargetVariant(lowercaseTarget)
	return true, nil
}

func (a *CIDMigrationAdapter) verifyDigest(ctx context.Context, actualName, expectedDigest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reader, err := a.ops.open(filepath.Join(a.basePath, actualName))
	if err != nil {
		return fmt.Errorf("open target alias %s: %w", actualName, err)
	}
	actual, err := service.HashReadCloser(ctx, reader)
	if err != nil {
		return fmt.Errorf("hash target alias %s: %w", actualName, err)
	}
	if actual != expectedDigest {
		return fmt.Errorf("target alias %s hashes to %s, want %s", actualName, actual, expectedDigest)
	}
	return nil
}

// FinalizeCIDAlias moves one already-unreferenced alias to parking. A
// case-insensitive SHA3 spelling first receives its parking hard link and is
// then renamed through a private intermediate name to make the active dirent
// exactly lowercase.
func (a *CIDMigrationAdapter) FinalizeCIDAlias(ctx context.Context, alias port.CIDStorageAlias, lowercaseTarget string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isValidExternalID(alias.Name) || !isLowerSHA3(lowercaseTarget) {
		return fmt.Errorf("finalize alias %q for %q: %w", alias.Name, lowercaseTarget, port.ErrInvalidKey)
	}
	if alias.Name == lowercaseTarget {
		return fmt.Errorf("refusing to park canonical target %s", lowercaseTarget)
	}
	parkingRoot := filepath.Join(a.basePath, filepath.FromSlash(cidSweepParkingDir))
	if err := a.ops.mkdirAll(parkingRoot, 0o750); err != nil {
		return fmt.Errorf("create CID sweep parking: %w", err)
	}
	activePath := filepath.Join(a.basePath, alias.Name)
	parkedPath := filepath.Join(parkingRoot, alias.Name)

	if alias.CanonicalizeCase {
		return a.finalizeCaseAlias(alias.Name, lowercaseTarget, activePath, parkedPath)
	}
	return a.finalizeOrdinaryAlias(alias.Name, activePath, parkedPath)
}

func (a *CIDMigrationAdapter) finalizeOrdinaryAlias(aliasName, activePath, parkedPath string) error {
	if _, err := a.ops.stat(parkedPath); err == nil {
		return fmt.Errorf("parked alias already exists: %s", aliasName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat parked alias %s: %w", aliasName, err)
	}
	if err := a.ops.rename(activePath, parkedPath); err != nil {
		return fmt.Errorf("park alias %s: %w", aliasName, err)
	}
	a.removeTargetVariant(aliasName)
	return nil
}

func (a *CIDMigrationAdapter) finalizeCaseAlias(aliasName, lowercaseTarget, activePath, parkedPath string) error {
	if !isSHA3(aliasName) || !strings.EqualFold(aliasName, lowercaseTarget) {
		return fmt.Errorf("invalid case-canonicalization alias %s for %s", aliasName, lowercaseTarget)
	}
	if err := a.linkParking(activePath, parkedPath); err != nil {
		return err
	}
	intermediate := filepath.Join(a.basePath, ".cid-sweep-case-"+lowercaseTarget)
	if _, err := a.ops.stat(intermediate); err == nil {
		return fmt.Errorf("case-canonicalization intermediate already exists: %s", intermediate)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat case-canonicalization intermediate: %w", err)
	}
	if err := a.ops.rename(activePath, intermediate); err != nil {
		return fmt.Errorf("rename case alias %s to intermediate: %w", aliasName, err)
	}
	if err := a.ops.rename(intermediate, filepath.Join(a.basePath, lowercaseTarget)); err != nil {
		_ = a.ops.rename(intermediate, activePath)
		return fmt.Errorf("rename case alias %s to lowercase target: %w", aliasName, err)
	}
	a.removeTargetVariant(aliasName)
	a.addTargetVariant(lowercaseTarget)
	return nil
}

func (a *CIDMigrationAdapter) linkParking(activePath, parkedPath string) error {
	if err := a.ops.link(activePath, parkedPath); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return fmt.Errorf("link alias into parking: %w", err)
	}
	activeInfo, activeErr := a.ops.stat(activePath)
	parkedInfo, parkedErr := a.ops.stat(parkedPath)
	if activeErr != nil || parkedErr != nil || !os.SameFile(activeInfo, parkedInfo) {
		return fmt.Errorf("parked alias exists with different file identity")
	}
	return nil
}

func isSHA3(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func isLowerSHA3(value string) bool {
	return isSHA3(value) && value == strings.ToLower(value)
}

var _ port.CIDMigrationStorage = (*CIDMigrationAdapter)(nil)
