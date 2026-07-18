package alkemiodb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://synapse:synapse@localhost:5432/alkemio?sslmode=disable" //nolint:gosec // test credentials
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("DB not available: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("DB not reachable: %v", err)
	}
	return pool
}

func TestGetByID_ExistingDocument(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	// Use a known document from the DB
	rows, err := pool.Query(context.Background(), `SELECT id FROM file LIMIT 1`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	if !rows.Next() {
		t.Skip("no documents in DB")
	}
	var id [16]byte
	if err := rows.Scan(&id); err != nil {
		t.Fatal(err)
	}
	docID := uuid.UUID(id)

	doc, err := a.GetByID(context.Background(), docID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if doc.ID != docID {
		t.Errorf("ID = %v, want %v", doc.ID, docID)
	}
	if doc.ExternalID == "" {
		t.Error("empty externalID")
	}
}

func TestGetByID_NotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	_, err := a.GetByID(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error for non-existent document")
	}
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestCreateAndDelete(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	// We need valid FK references. Get a storageBucket and authorization from an existing doc.
	var storageBucketID, authorizationID [16]byte
	err := pool.QueryRow(context.Background(),
		`SELECT "storageBucketId", "authorizationId" FROM file WHERE "storageBucketId" IS NOT NULL AND "authorizationId" IS NOT NULL LIMIT 1`,
	).Scan(&storageBucketID, &authorizationID)
	if err != nil {
		t.Skipf("no document with valid FKs: %v", err)
	}

	// Create a new authorization_policy for our test document (to avoid unique constraint)
	var newAuthID [16]byte
	newAuthUUID, _ := uuid.NewV7()
	err = pool.QueryRow(context.Background(),
		`INSERT INTO authorization_policy (id, "credentialRules", "privilegeRules", type, version)
		 VALUES ($1, '[]', '[]', 'document', 1) RETURNING id`,
		newAuthUUID,
	).Scan(&newAuthID)
	if err != nil {
		t.Skipf("cannot create test auth policy: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, newAuthUUID)
	}()

	docID, _ := uuid.NewV7()
	now := time.Now()
	doc := model.Document{
		ID:                docID,
		ExternalID:        "test-hash-" + docID.String()[:8],
		MimeType:          "text/plain",
		Size:              42,
		DisplayName:       "test-create.txt",
		TemporaryLocation: false,
		StorageBucketID:   uuid.UUID(storageBucketID),
		AuthorizationID:   newAuthUUID,
		CreatedDate:       now,
		UpdatedDate:       now,
	}

	createdID, err := a.Create(context.Background(), doc, model.ContentMetadata{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if createdID != docID {
		t.Errorf("created ID = %v, want %v", createdID, docID)
	}

	// Verify it exists
	fetched, err := a.GetByID(context.Background(), docID)
	if err != nil {
		t.Fatalf("GetByID after create: %v", err)
	}
	if fetched.DisplayName != "test-create.txt" {
		t.Errorf("displayName = %q", fetched.DisplayName)
	}

	// Delete it
	deleted, err := a.Delete(context.Background(), docID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.AuthorizationID != newAuthUUID {
		t.Errorf("authorizationID = %v, want %v", deleted.AuthorizationID, newAuthUUID)
	}

	// Verify gone
	_, err = a.GetByID(context.Background(), docID)
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected not found after delete, got %v", err)
	}
}

func TestUpdateFile(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	// Get a real document to update
	var id [16]byte
	var origExtID, origMime string
	var origSize int32
	err := pool.QueryRow(context.Background(),
		`SELECT id, "externalID", "mimeType", size FROM file LIMIT 1`,
	).Scan(&id, &origExtID, &origMime, &origSize)
	if err != nil {
		t.Skip("no documents")
	}
	docID := uuid.UUID(id)

	newExtID := "updated-" + uuid.New().String()[:8]
	err = a.UpdateFile(context.Background(), docID, newExtID, "text/plain", 99, model.ContentMetadata{})
	if err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}

	// Restore original values
	defer func() {
		_ = a.UpdateFile(context.Background(), docID, origExtID, origMime, int(origSize), model.ContentMetadata{})
	}()

	doc, _ := a.GetByID(context.Background(), docID)
	if doc.ExternalID != newExtID {
		t.Errorf("externalID = %q, want %q", doc.ExternalID, newExtID)
	}
}

func TestUpdateFile_NotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	err := a.UpdateFile(context.Background(), uuid.New(), "hash", "text/plain", 1, model.ContentMetadata{})
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestCountByExternalID(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	// Known hash from existing data
	var extID string
	err := pool.QueryRow(context.Background(),
		`SELECT "externalID" FROM file LIMIT 1`,
	).Scan(&extID)
	if err != nil {
		t.Skip("no documents")
	}

	count, err := a.CountByExternalID(context.Background(), extID)
	if err != nil {
		t.Fatalf("CountByExternalID: %v", err)
	}
	if count < 1 {
		t.Errorf("count = %d, expected >= 1", count)
	}

	// Non-existent hash
	count, err = a.CountByExternalID(context.Background(), "definitely-not-a-real-hash")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestUpdateMetadata(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	// Seed a DEDICATED row (its own auth policy, a borrowed bucket FK) instead of
	// picking a random shared row. UpdateMetadata's UPDATE unconditionally SETs
	// createdBy/externalReference from the passed struct; run against a shared row
	// with a partial update (CreatedBy/ExternalReference nil) it would NULL those
	// columns on real fixture data — corrupting it for other tests. (Production is
	// safe: the handler's buildMetadataUpdate fills absent createdBy/externalReference
	// with the row's current values, so an omitted field is a no-op SET, never a
	// clear.) cleanup tears this row down, so no restore is needed.
	docID, _, cleanup := createTestRow(t, pool, nil)
	defer cleanup()

	// Read the seeded row's current mutable fields for the optimistic-lock update.
	var bucketID, authID [16]byte
	var tempLoc bool
	var displayName string
	var version int32
	if err := pool.QueryRow(context.Background(),
		`SELECT "storageBucketId", "authorizationId", "temporaryLocation", "displayName", version FROM file WHERE id = $1`,
		docID,
	).Scan(&bucketID, &authID, &tempLoc, &displayName, &version); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	origBucket := uuid.UUID(bucketID)
	origAuth := uuid.UUID(authID)

	// Update with current version (optimistic lock). Preserve the row's
	// authorizationId so this test does not mutate auth/ownership. UpdateMetadata
	// maps the row's authoritative post-update document from the full RETURNING;
	// assert its content identity is non-empty AND the applied SETs are reflected
	// (proving the whole row — not just externalID/size — comes back).
	updated, err := a.UpdateMetadata(context.Background(), docID, model.DocumentMetadataUpdate{
		StorageBucketID: origBucket, TemporaryLocation: !tempLoc, DisplayName: displayName, AuthorizationID: &origAuth,
	}, int(version))
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if updated.ExternalID == "" {
		t.Error("UpdateMetadata returned an empty externalID from RETURNING")
	}
	if updated.TemporaryLocation != !tempLoc || updated.DisplayName != displayName {
		t.Errorf("returned doc = {temp:%v, name:%q}, want the applied SETs {temp:%v, name:%q}",
			updated.TemporaryLocation, updated.DisplayName, !tempLoc, displayName)
	}

	doc, _ := a.GetByID(context.Background(), docID)
	if doc.TemporaryLocation != !tempLoc {
		t.Errorf("temporaryLocation = %v, want %v", doc.TemporaryLocation, !tempLoc)
	}
}

func TestUpdateMetadata_NotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	newAuth := uuid.New()
	_, err := a.UpdateMetadata(context.Background(), uuid.New(), model.DocumentMetadataUpdate{
		StorageBucketID: uuid.New(), DisplayName: "name.txt", AuthorizationID: &newAuth,
	}, 1)
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	_, err := a.Delete(context.Background(), uuid.New())
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

// --- Phase 7: BackfillContentMetadata integration tests (FR-018) ---

// createTestRow creates a real `file` row (with valid FKs) so the test
// can exercise BackfillContentMetadata against a row that actually
// exists. Returns the row's ID, the row's externalID (needed for the
// compare-and-set backfill), and a teardown function. rawContentMetadata
// is injected verbatim into the JSONB column so tests can seed forward-
// fit shapes the typed Create path would not produce.
func createTestRow(t *testing.T, pool *pgxpool.Pool, rawContentMetadata []byte) (uuid.UUID, string, func()) {
	t.Helper()
	a := New(pool)

	// Need valid FK references — pull from an existing row.
	var storageBucketID [16]byte
	if err := pool.QueryRow(context.Background(),
		`SELECT "storageBucketId" FROM file WHERE "storageBucketId" IS NOT NULL LIMIT 1`,
	).Scan(&storageBucketID); err != nil {
		t.Skipf("no existing storage bucket FK to borrow: %v", err)
	}

	authUUID, _ := uuid.NewV7()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO authorization_policy (id, "credentialRules", "privilegeRules", type, version)
		 VALUES ($1, '[]', '[]', 'document', 1)`,
		authUUID,
	); err != nil {
		t.Skipf("cannot create test auth policy: %v", err)
	}

	docID, _ := uuid.NewV7()
	externalID := "backfill-test-" + docID.String()[:8]
	now := time.Now()
	doc := model.Document{
		ID:                docID,
		ExternalID:        externalID,
		MimeType:          "image/png",
		Size:              123,
		DisplayName:       "backfill-test.png",
		TemporaryLocation: false,
		StorageBucketID:   uuid.UUID(storageBucketID),
		AuthorizationID:   authUUID,
		CreatedDate:       now,
		UpdatedDate:       now,
	}
	if _, err := a.Create(context.Background(), doc, model.ContentMetadata{}); err != nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
		t.Fatalf("Create test row: %v", err)
	}
	// Inject the requested raw content_metadata verbatim. Bypass Create's
	// typed marshaller so callers can seed forward-fit shapes (e.g. with
	// unknown keys for FR-017 coverage).
	if len(rawContentMetadata) > 0 && string(rawContentMetadata) != `{}` {
		if _, err := pool.Exec(context.Background(),
			`UPDATE file SET content_metadata = $1 WHERE id = $2`,
			rawContentMetadata, docID,
		); err != nil {
			_, _ = a.Delete(context.Background(), docID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
			t.Fatalf("seed content_metadata: %v", err)
		}
	}
	return docID, externalID, func() {
		_, _ = a.Delete(context.Background(), docID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
	}
}

// SC-011: BackfillContentMetadata writes content_metadata without bumping
// the version column.
func TestAdapter_BackfillContentMetadata_DoesNotBumpVersion(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	docID, externalID, cleanup := createTestRow(t, pool, []byte(`{}`))
	defer cleanup()

	// Read initial version.
	var versionBefore int32
	if err := pool.QueryRow(context.Background(),
		`SELECT version FROM file WHERE id = $1`, docID,
	).Scan(&versionBefore); err != nil {
		t.Fatalf("read version before: %v", err)
	}

	w, h := 100, 50
	if err := a.BackfillContentMetadata(
		context.Background(), docID, externalID,
		model.ContentMetadata{Populated: true, ImageWidth: &w, ImageHeight: &h},
	); err != nil {
		t.Fatalf("BackfillContentMetadata: %v", err)
	}

	// Verify content_metadata changed AND version did not bump.
	var versionAfter int32
	var meta []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT version, "content_metadata" FROM file WHERE id = $1`, docID,
	).Scan(&versionAfter, &meta); err != nil {
		t.Fatalf("read after backfill: %v", err)
	}
	if versionAfter != versionBefore {
		t.Errorf("version bumped: before=%d, after=%d (FR-018 disjoint-write)", versionBefore, versionAfter)
	}
	// Postgres JSONB normalizes whitespace; just check the field is present.
	if !bytes.Contains(meta, []byte(`"imageWidth"`)) || !bytes.Contains(meta, []byte(`100`)) {
		t.Errorf("content_metadata = %s, want imageWidth=100", meta)
	}
}

func TestAdapter_BackfillContentMetadata_IsIdempotent(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	docID, externalID, cleanup := createTestRow(t, pool, []byte(`{}`))
	defer cleanup()

	w, h := 200, 75
	payload := model.ContentMetadata{Populated: true, ImageWidth: &w, ImageHeight: &h}
	// First call wins the compare-and-set; second is a 0-rows-affected no-op
	// (content_metadata is no longer empty), which the adapter treats as
	// success — i.e. the helper is safe to call repeatedly.
	if err := a.BackfillContentMetadata(context.Background(), docID, externalID, payload); err != nil {
		t.Fatalf("first BackfillContentMetadata: %v", err)
	}
	if err := a.BackfillContentMetadata(context.Background(), docID, externalID, payload); err != nil {
		t.Fatalf("second BackfillContentMetadata: %v", err)
	}

	var meta []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT "content_metadata" FROM file WHERE id = $1`, docID,
	).Scan(&meta); err != nil {
		t.Fatalf("read after backfill: %v", err)
	}
	if !bytes.Contains(meta, []byte(`"imageWidth"`)) || !bytes.Contains(meta, []byte(`200`)) {
		t.Errorf("content_metadata = %s, want imageWidth=200", meta)
	}
}

// SC-011 (race protection): BackfillContentMetadata is a compare-and-set on
// (id, externalID, empty content_metadata). When the row's externalID has
// changed (Replace ran between measure and persist), the write is skipped
// and the existing fresh metadata is preserved.
func TestAdapter_BackfillContentMetadata_RaceLost_DoesNotOverwrite(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	docID, externalID, cleanup := createTestRow(t, pool, []byte(`{}`))
	defer cleanup()

	// Simulate "Replace ran" by changing the row's externalID after the
	// lazy-backfill measured against the original.
	if _, err := pool.Exec(context.Background(),
		`UPDATE file SET "externalID" = $1 WHERE id = $2`,
		externalID+"-replaced", docID,
	); err != nil {
		t.Fatalf("simulate replace: %v", err)
	}

	w, h := 100, 50
	if err := a.BackfillContentMetadata(
		context.Background(), docID, externalID, // expectedExternalID = pre-replace
		model.ContentMetadata{Populated: true, ImageWidth: &w, ImageHeight: &h},
	); err != nil {
		t.Fatalf("BackfillContentMetadata returned err on race-lost (should be nil): %v", err)
	}

	var meta []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT "content_metadata" FROM file WHERE id = $1`, docID,
	).Scan(&meta); err != nil {
		t.Fatalf("read after backfill: %v", err)
	}
	// Compare-and-set rejected the write because externalID didn't match —
	// the column is still the seeded empty value.
	if string(meta) != `{}` {
		t.Errorf("content_metadata overwrote despite externalID mismatch: %s", meta)
	}
}

// FR-017 / COV-2: GetByID must tolerate unknown JSON keys in
// content_metadata and still extract imageWidth/imageHeight.
func TestAdapter_GetByID_TolerantOfUnknownContentMetadataKeys(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	forwardFit := []byte(`{"imageWidth":100,"imageHeight":50,"_futureField":"x","videoDuration":1.5}`)
	docID, _, cleanup := createTestRow(t, pool, forwardFit)
	defer cleanup()

	doc, err := a.GetByID(context.Background(), docID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if doc.ImageWidth == nil || *doc.ImageWidth != 100 {
		t.Errorf("ImageWidth = %v, want 100", doc.ImageWidth)
	}
	if doc.ImageHeight == nil || *doc.ImageHeight != 50 {
		t.Errorf("ImageHeight = %v, want 50", doc.ImageHeight)
	}
}
