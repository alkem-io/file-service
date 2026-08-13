package alkemiodb

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// createLegacyRow inserts one row naming a given externalID, against a real
// database (018-legacy-cid-normalization).
func createLegacyRow(t *testing.T, pool *pgxpool.Pool, externalID string) (uuid.UUID, func()) {
	t.Helper()
	a := New(pool)

	var storageBucketID [16]byte
	if err := pool.QueryRow(context.Background(),
		`SELECT "storageBucketId" FROM file WHERE "storageBucketId" IS NOT NULL LIMIT 1`,
	).Scan(&storageBucketID); err != nil {
		t.Skipf("no existing storage bucket FK to borrow: %v", err)
	}
	authUUID, _ := uuid.NewV7()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO authorization_policy (id, "credentialRules", "privilegeRules", type, version)
		 VALUES ($1, '[]', '[]', 'document', 1)`, authUUID,
	); err != nil {
		t.Skipf("cannot create test auth policy: %v", err)
	}

	docID, _ := uuid.NewV7()
	now := time.Now()
	doc := model.Document{
		ID: docID, ExternalID: externalID, MimeType: "image/png", Size: 123,
		DisplayName: "sweep-cids-test.png", StorageBucketID: uuid.UUID(storageBucketID),
		AuthorizationID: authUUID, CreatedDate: now, UpdatedDate: now,
	}
	if _, err := a.Create(context.Background(), doc, model.ContentMetadata{}); err != nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
		t.Fatalf("create test row: %v", err)
	}
	return docID, func() {
		_, _ = a.Delete(context.Background(), docID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
	}
}

// The rename is the migration. Against a real database, because the two ways it can
// be wrong — moving the wrong rows, or moving them in the wrong direction — both
// compile and both are invisible to a fake.
func TestRenameExternalID(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	legacy := "QmRenameTest" + strings.Repeat("X", 32)
	digest := strings.Repeat("c", 64)
	other := "QmUnrelated" + strings.Repeat("Y", 32)

	// Three rows share the legacy name; one names something else entirely.
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		id, cleanup := createLegacyRow(t, pool, legacy)
		defer cleanup()
		ids = append(ids, id)
	}
	otherID, cleanupOther := createLegacyRow(t, pool, other)
	defer cleanupOther()

	moved, err := a.RenameExternalID(context.Background(), legacy, digest)
	if err != nil {
		t.Fatalf("RenameExternalID: %v", err)
	}
	if moved != 3 {
		t.Errorf("moved %d rows, want 3 — one statement must move EVERY row naming the blob", moved)
	}

	// Direction matters, and an argument swap compiles: it would rename digests back
	// to CIDs, undoing the migration on every run.
	for _, id := range ids {
		got, err := a.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ExternalID != digest {
			t.Errorf("row %s names %q, want %q", id, got.ExternalID, digest)
		}
	}

	// Rows naming anything else are untouched — the guard is `WHERE "externalID" = $1`
	// and nothing else, so a predicate mistake shows up here.
	untouched, err := a.GetByID(context.Background(), otherID)
	if err != nil {
		t.Fatalf("read back other: %v", err)
	}
	if untouched.ExternalID != other {
		t.Errorf("an unrelated row was renamed to %q", untouched.ExternalID)
	}
}

// Zero rows is the normal outcome for an unreferenced blob, and for one whose rows a
// concurrent writer already moved. It must not be an error — the sweep branches on
// the count, and an error would turn "nothing to do" into a failed run.
func TestRenameExternalID_NoMatchingRowsIsNotAnError(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	moved, err := a.RenameExternalID(context.Background(),
		"QmNothingNamesThis"+strings.Repeat("Z", 32), strings.Repeat("d", 64))
	if err != nil {
		t.Fatalf("RenameExternalID on an unreferenced name: %v", err)
	}
	if moved != 0 {
		t.Errorf("moved %d rows for a name nothing references, want 0", moved)
	}
}

// The rename must not disturb anything describing the bytes: they did not change.
// updatedDate in particular — churning it across the cohort would corrupt the audit
// trail and any recently-modified ordering.
func TestRenameExternalID_TouchesOnlyTheName(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	legacy := "QmUntouchedTest" + strings.Repeat("W", 32)
	id, cleanup := createLegacyRow(t, pool, legacy)
	defer cleanup()

	before, err := a.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	if _, err := a.RenameExternalID(context.Background(), legacy, strings.Repeat("e", 64)); err != nil {
		t.Fatalf("RenameExternalID: %v", err)
	}
	after, err := a.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}

	if after.MimeType != before.MimeType || after.Size != before.Size {
		t.Errorf("content fields moved: mime %q->%q size %d->%d",
			before.MimeType, after.MimeType, before.Size, after.Size)
	}
	if !after.UpdatedDate.Equal(before.UpdatedDate) {
		t.Errorf("updatedDate moved %v -> %v; a rename is not a user edit",
			before.UpdatedDate, after.UpdatedDate)
	}
	if after.DisplayName != before.DisplayName {
		t.Errorf("displayName moved %q -> %q", before.DisplayName, after.DisplayName)
	}
}
