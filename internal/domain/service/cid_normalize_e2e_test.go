//go:build e2e

// Package service_test drives the sweep through its REAL adapters — the local
// filesystem store and the pgx/sqlc repository — against a real directory and a real
// database.
//
// It exists because the fake-based tests share the implementation's blind spots. They
// are written against the same model of the world the code holds, so a thing the code
// never thought to do is a thing they never think to assert. Two defects reached a
// clean lint, a clean race-enabled suite and five review rounds before a manual run of
// the binary found them:
//
//   - the parked orphan appeared only as a COUNT, so `_parked/` held a file the report
//     could not account for — on the one directory whose purpose is to be reversible;
//   - the case-identity bug shipped behind a test that asserted the case-insensitive
//     outcome against a map-backed (case-sensitive) fake.
//
// Run with: go test -tags e2e ./internal/domain/service/
package service_test

import (
	"context"
	"crypto/sha3"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/adapter/outbound/alkemiodb"
	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/domain/service"
)

func digestOf(b []byte) string {
	h := sha3.New256()
	_, _ = h.Write(b) // hash.Hash never errors
	return hex.EncodeToString(h.Sum(nil))
}

func e2ePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// Skipping is only honest when nobody ASKED for a database. CI sets
	// TEST_DATABASE_URL, so an unreachable one there means the service container
	// broke — and skipping would turn this into a green job asserting nothing, which
	// is the exact failure this file's header is about.
	dsn, required := os.LookupEnv("TEST_DATABASE_URL")
	if dsn == "" {
		required = false
		dsn = "postgres://synapse:synapse@localhost:5432/alkemio?sslmode=disable" //nolint:gosec // test credentials
	}
	fail := t.Skipf
	if required {
		fail = t.Fatalf
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		fail("test database: %v", err)
		return nil
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		fail("test database unreachable: %v", err)
		return nil
	}
	return pool
}

// seedRow inserts one FIXTURE row naming externalID. Prerequisites are created, not
// borrowed, and setup failures are fatal — a fixture that skips itself lets the whole
// scenario report green having exercised nothing.
func seedRow(t *testing.T, pool *pgxpool.Pool, bucket uuid.UUID, externalID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	auth, _ := uuid.NewV7()
	if _, err := pool.Exec(ctx,
		`INSERT INTO authorization_policy (id,"credentialRules","privilegeRules",type,version)
		 VALUES ($1,'[]','[]','document',1)`, auth); err != nil {
		t.Fatalf("seed auth policy: %v", err)
	}
	id, _ := uuid.NewV7()
	if _, err := pool.Exec(ctx,
		`INSERT INTO file (id,"externalID","mimeType",size,"displayName","createdBy","temporaryLocation",
		    "storageBucketId","authorizationId","createdDate","updatedDate",version,content_metadata)
		 VALUES ($1,$2,'image/png',14,'e2e-sweep-cids.png',NULL,false,$3,$4,now(),now(),1,'{}')`,
		id, externalID, bucket, auth); err != nil {
		t.Fatalf("seed file row: %v", err)
	}
	return id
}

// TestSweepCIDsEndToEnd runs the three populations the migration actually meets, with
// the real adapters, and asserts the properties an operator depends on.
func TestSweepCIDsEndToEnd(t *testing.T) {
	pool := e2ePool(t)
	defer pool.Close()
	ctx := context.Background()

	store := t.TempDir()
	sharedBytes, singleBytes, orphanBytes := []byte("shared payload"), []byte("single payload"), []byte("orphan payload")
	normalized := []byte("already normalized")

	const sharedCID = "QmSharedSharedSharedSharedSharedSharedAA"
	const singleCID = "QmSingleSingleSingleSingleSingleSingle"
	const orphanCID = "QmOrphanOrphanOrphanOrphanOrphanOrphan"

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(store, name), b, 0o600); err != nil {
			t.Fatalf("seed blob %s: %v", name, err)
		}
	}
	write(sharedCID, sharedBytes)
	write(singleCID, singleBytes)
	write(orphanCID, orphanBytes)           // referenced by NOTHING
	write(digestOf(normalized), normalized) // control: already correct, must be untouched

	bucket, _ := uuid.NewV7()
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_bucket (id,version,"allowedMimeTypes","maxFileSize") VALUES ($1,1,'{}',10485760)`,
		bucket); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	// Teardown is scoped to THIS run's bucket, not to a global displayName: two
	// interleaved runs against a shared integration database would otherwise delete
	// each other's rows mid-test. The authorization_policy rows are removed too —
	// leaking one per seeded file on every run is how a shared database rots.
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM authorization_policy WHERE id IN (
			SELECT "authorizationId" FROM file WHERE "storageBucketId" = $1)`, bucket)
		_, _ = pool.Exec(ctx, `DELETE FROM file WHERE "storageBucketId" = $1`, bucket)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_bucket WHERE id = $1`, bucket)
	}()

	// Two rows share one CID — the population the whole blob-oriented design is for.
	seedRow(t, pool, bucket, sharedCID)
	seedRow(t, pool, bucket, sharedCID)
	seedRow(t, pool, bucket, singleCID)

	svc := &service.FileService{
		Repo:        alkemiodb.New(pool),
		Storage:     local.New(store),
		LegacyStore: local.New(store),
		Logger:      zap.NewNop(),
	}
	sink := local.NewReportSink(store, local.ReservedReportDir)
	opts := service.CIDNormalizeOptions{Rate: 10_000, Report: sink}

	assertPreviewChangesNothing(t, ctx, svc, opts, store)

	// --- the real pass ---
	sum := svc.RunCIDNormalize(ctx, opts)
	if sum.Failed != 0 || sum.Aborted || sum.ReportFailed {
		t.Fatalf("pass did not complete cleanly: %+v", sum)
	}
	if sum.Normalized != 2 || sum.Records != 3 || sum.Orphans != 1 || sum.Parked != 3 {
		t.Errorf("normalized=%d records=%d orphans=%d parked=%d; want 2/3/1/3",
			sum.Normalized, sum.Records, sum.Orphans, sum.Parked)
	}

	assertEveryRowResolves(t, ctx, pool, bucket, store, 3)
	assertContentNamespaceIsClean(t, store, digestOf(normalized))
	// An orphan under a DIGEST name no longer matches the scan, so no future pass
	// could ever find it — strictly worse than the legacy-named one it replaced.
	if _, err := os.Stat(filepath.Join(store, digestOf(orphanBytes))); err == nil {
		t.Error("published a digest-named blob for content nothing references")
	}
	assertParkedDirIsAccountedFor(t, store, sum)

	// --- re-running a converged corpus is a no-op ---
	again := svc.RunCIDNormalize(ctx, opts)
	if again.Normalized != 0 || again.Orphans != 0 || again.Parked != 0 {
		t.Errorf("re-run was not a no-op: %+v", again)
	}
}

// assertEveryRowResolves is THE invariant: after the pass, no row names content that
// is not on the store.
func assertEveryRowResolves(t *testing.T, ctx context.Context, pool *pgxpool.Pool, bucket uuid.UUID, store string, want int) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT "externalID" FROM file WHERE "storageBucketId" = $1`, bucket)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			t.Fatal(err)
		}
		seen++
		if _, err := os.Stat(filepath.Join(store, ext)); err != nil {
			t.Errorf("row names %q, which is not on the store: %v", ext, err)
		}
	}
	if seen != want {
		t.Errorf("read back %d rows, want %d", seen, want)
	}
}

// assertContentNamespaceIsClean checks the migration converged and took nothing with
// it: only digest-named files remain, and the already-correct control is untouched.
func assertContentNamespaceIsClean(t *testing.T, store, controlDigest string) {
	t.Helper()
	entries, err := os.ReadDir(store)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) != 64 {
			t.Errorf("legacy-named file %q remains in the content namespace", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(store, controlDigest)); err != nil {
		t.Errorf("the already-normalized control blob was disturbed: %v", err)
	}
}

// assertParkedDirIsAccountedFor is the reconciliation an operator performs to undo the
// migration: every file in _parked/ must be explained by the run that put it there.
// A count alone does not satisfy it — that gap is why this test exists.
func assertParkedDirIsAccountedFor(t *testing.T, store string, sum service.CIDNormalizeSummary) {
	t.Helper()
	parked, err := os.ReadDir(filepath.Join(store, local.ReservedParkedDir))
	if err != nil {
		t.Fatalf("read parked dir: %v", err)
	}
	if len(parked) != 3 {
		t.Errorf("_parked/ holds %d files, want 3 — nothing is deleted", len(parked))
	}
	accounted := map[string]bool{}
	for _, c := range sum.Changed {
		accounted[c.PreviousExternalID] = true
	}
	for _, o := range sum.ParkedOrphans {
		accounted[o] = true
	}
	for _, e := range parked {
		if !accounted[e.Name()] {
			t.Errorf("%q sits in the migration's undo directory with nothing in the run accounting for it", e.Name())
		}
	}
}

// assertPreviewChangesNothing pins the approval gate: the preview counts what a real
// pass would touch and writes nothing at all.
func assertPreviewChangesNothing(t *testing.T, ctx context.Context, svc *service.FileService,
	opts service.CIDNormalizeOptions, store string) {
	t.Helper()
	dry := opts
	dry.DryRun = true
	preview := svc.RunCIDNormalize(ctx, dry)
	if preview.WouldNormalize != 3 {
		t.Errorf("wouldNormalize = %d, want 3 (two referenced CIDs + one orphan; the normalized blob is out of scope)",
			preview.WouldNormalize)
	}
	if entries, _ := os.ReadDir(filepath.Join(store, local.ReservedParkedDir)); len(entries) != 0 {
		t.Error("the dry run parked something")
	}
}
