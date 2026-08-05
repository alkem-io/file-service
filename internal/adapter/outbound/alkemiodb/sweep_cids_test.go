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

// createSweepRow inserts one row with a caller-chosen externalID and temporary
// flag — the two dimensions ListLegacyNamed's predicate discriminates on
// (018-legacy-cid-normalization).
func createSweepRow(t *testing.T, pool *pgxpool.Pool, externalID string, temporary bool) (uuid.UUID, func()) {
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
		ID:                docID,
		ExternalID:        externalID,
		MimeType:          "image/png",
		Size:              123,
		DisplayName:       "sweep-cids-test.png",
		TemporaryLocation: temporary,
		StorageBucketID:   uuid.UUID(storageBucketID),
		AuthorizationID:   authUUID,
		CreatedDate:       now,
		UpdatedDate:       now,
	}
	if _, err := a.Create(context.Background(), doc, model.ContentMetadata{}); err != nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
		t.Fatalf("create sweep test row: %v", err)
	}
	return docID, func() {
		_, _ = a.Delete(context.Background(), docID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, authUUID)
	}
}

// listAllLegacy drains the keyset pages so a test can assert on membership
// without depending on where its rows land relative to the rest of the corpus.
func listAllLegacy(t *testing.T, a *Adapter) map[uuid.UUID]model.Document {
	t.Helper()
	out := map[uuid.UUID]model.Document{}
	cursor := uuid.Nil
	for {
		page, err := a.ListLegacyNamed(context.Background(), cursor, 500)
		if err != nil {
			t.Fatalf("ListLegacyNamed: %v", err)
		}
		if len(page) == 0 {
			return out
		}
		for _, d := range page {
			out[d.ID] = d
			cursor = d.ID
		}
	}
}

// The predicate is the whole scope of the sweep: permanent rows whose externalID
// is not the current content-addressing scheme. Getting either half wrong either
// misses data-at-risk or rewrites rows that are already correct.
func TestListLegacyNamed_PredicateScope(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	validHex := strings.Repeat("a", 64) // a well-formed SHA3-256 name — must NOT match
	legacyCID := "QmSweepCIDsTestXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

	legacyPermanent, c1 := createSweepRow(t, pool, legacyCID, false)
	defer c1()
	legacyTemporary, c2 := createSweepRow(t, pool, legacyCID+"tmp", true)
	defer c2()
	alreadyNormalized, c3 := createSweepRow(t, pool, validHex, false)
	defer c3()

	got := listAllLegacy(t, a)

	if _, ok := got[legacyPermanent]; !ok {
		t.Error("a permanent row with a legacy name was NOT selected — the sweep would miss data-at-risk")
	}
	if _, ok := got[legacyTemporary]; ok {
		t.Error("a TEMPORARY row was selected; a row still flagged temporary in this cohort is orphaned residue and out of scope")
	}
	if _, ok := got[alreadyNormalized]; ok {
		t.Error("a row already named by a 64-char hex digest was selected — the sweep would rewrite correct rows and never converge")
	}
}

// Only the five columns the sweep needs are projected; the rest stay zero rather
// than being invented by the mapper.
func TestListLegacyNamed_ProjectsOnlyWhatTheSweepNeeds(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	id, cleanup := createSweepRow(t, pool, "QmProjectionTestXXXXXXXXXXXXXXXXXXXXXXXXXXX", false)
	defer cleanup()

	doc, ok := listAllLegacy(t, a)[id]
	if !ok {
		t.Fatal("seeded row not returned")
	}
	if doc.ExternalID == "" || doc.MimeType == "" || doc.Size == 0 || doc.Version == 0 {
		t.Errorf("identity/guard fields not populated: %+v", doc)
	}
	if doc.DisplayName != "" || !doc.CreatedDate.IsZero() {
		t.Errorf("unprojected fields were populated: displayName=%q createdDate=%v", doc.DisplayName, doc.CreatedDate)
	}
}

// The compare-and-set is what lets the sweep run against a live platform.
func TestNormalizeExternalID_CompareAndSet(t *testing.T) {
	newName := strings.Repeat("b", 64)

	t.Run("applies when the row has not moved", func(t *testing.T) {
		pool := testPool(t)
		defer pool.Close()
		a := New(pool)
		id, cleanup := createSweepRow(t, pool, "QmCasApplyTestXXXXXXXXXXXXXXXXXXXXXXXXXXXX", false)
		defer cleanup()

		before, err := a.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read before: %v", err)
		}

		applied, err := a.NormalizeExternalID(context.Background(), id, before.ExternalID, before.Version, newName)
		if err != nil {
			t.Fatalf("NormalizeExternalID: %v", err)
		}
		if !applied {
			t.Fatal("compare-and-set did not apply on an unchanged row")
		}

		after, err := a.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("read after: %v", err)
		}
		if after.ExternalID != newName {
			t.Errorf("externalID = %q, want %q", after.ExternalID, newName)
		}
		if after.Version != before.Version+1 {
			t.Errorf("version = %d, want %d — identity changed, so other CAS holders must lose", after.Version, before.Version+1)
		}
		// A rename is not a user edit: everything describing the bytes must be untouched.
		if after.MimeType != before.MimeType || after.Size != before.Size {
			t.Errorf("content fields moved: mime %q->%q size %d->%d", before.MimeType, after.MimeType, before.Size, after.Size)
		}
		if !after.UpdatedDate.Equal(before.UpdatedDate) {
			t.Errorf("updatedDate moved %v -> %v; churning it across the cohort would corrupt the audit trail",
				before.UpdatedDate, after.UpdatedDate)
		}
	})

	t.Run("loses to a concurrent content change", func(t *testing.T) {
		pool := testPool(t)
		defer pool.Close()
		a := New(pool)
		id, cleanup := createSweepRow(t, pool, "QmCasNameMovedTestXXXXXXXXXXXXXXXXXXXXXXXX", false)
		defer cleanup()

		before, _ := a.GetByID(context.Background(), id)
		applied, err := a.NormalizeExternalID(context.Background(), id, "QmSomethingElseEntirelyXXXXXXXXXXXXXXXXXXX", before.Version, newName)
		if err != nil {
			t.Fatalf("a lost race must not be an error: %v", err)
		}
		if applied {
			t.Fatal("applied despite the externalID having moved — a concurrent writer's change would be lost")
		}
	})

	t.Run("loses to a concurrent version bump", func(t *testing.T) {
		pool := testPool(t)
		defer pool.Close()
		a := New(pool)
		id, cleanup := createSweepRow(t, pool, "QmCasVersionMovedTestXXXXXXXXXXXXXXXXXXXXX", false)
		defer cleanup()

		before, _ := a.GetByID(context.Background(), id)
		applied, err := a.NormalizeExternalID(context.Background(), id, before.ExternalID, before.Version+99, newName)
		if err != nil {
			t.Fatalf("a lost race must not be an error: %v", err)
		}
		if applied {
			t.Fatal("applied despite the version having moved")
		}
	})
}
