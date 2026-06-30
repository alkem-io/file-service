package alkemiodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// INTEGRATION INFRA REQUIRED. These tests exercise the externalReference column,
// the by-reference lookups, and the move (UpdateMetadata) primitive against a
// live Alkemio Postgres. They skip when no DB is reachable (testPool) — not a
// fake pass. Point TEST_DATABASE_URL at a seeded Alkemio DB to run them.

// newAuthPolicy inserts a throwaway document authorization_policy and returns
// its id plus a cleanup func, mirroring TestCreateAndDelete's FK handling
// (file."authorizationId" is UNIQUE, so every row needs its own policy).
func newAuthPolicy(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, func()) {
	t.Helper()
	id, _ := uuid.NewV7()
	var got [16]byte
	err := pool.QueryRow(context.Background(),
		`INSERT INTO authorization_policy (id, "credentialRules", "privilegeRules", type, version)
		 VALUES ($1, '[]', '[]', 'document', 1) RETURNING id`, id,
	).Scan(&got)
	if err != nil {
		t.Skipf("cannot create test auth policy: %v", err)
	}
	return id, func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM authorization_policy WHERE id = $1`, id)
	}
}

// twoBuckets returns two distinct existing storageBucket ids, or skips.
func twoBuckets(t *testing.T, raw *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	rows, err := raw.Query(context.Background(),
		`SELECT DISTINCT "storageBucketId" FROM file WHERE "storageBucketId" IS NOT NULL LIMIT 2`)
	if err != nil {
		t.Skipf("query buckets: %v", err)
	}
	defer rows.Close()
	var got []uuid.UUID
	for rows.Next() {
		var b [16]byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, uuid.UUID(b))
	}
	if len(got) < 2 {
		t.Skip("need at least two distinct storage buckets in the DB")
	}
	return got[0], got[1]
}

// T014: create → by-reference (global & scoped) → PATCH move (bucket + auth +
// createdBy + ref) → by-reference resolves in the new bucket; the old bucket
// misses; global still resolves.
func TestByReference_MoveResolution(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	stagingBucket, convBucket := twoBuckets(t, pool)
	authA, cleanupA := newAuthPolicy(t, pool)
	defer cleanupA()
	authB, cleanupB := newAuthPolicy(t, pool)
	defer cleanupB()

	ref := "media-id-" + uuid.New().String()
	externalID := "blob-" + uuid.New().String()[:12]
	docID, _ := uuid.NewV7()
	now := time.Now()

	if _, err := a.Create(context.Background(), model.Document{
		ID:                docID,
		ExternalID:        externalID,
		MimeType:          "image/jpeg",
		Size:              10,
		DisplayName:       "staged.jpg",
		StorageBucketID:   stagingBucket,
		AuthorizationID:   authA,
		ExternalReference: &ref,
		CreatedDate:       now,
		UpdatedDate:       now,
	}, model.ContentMetadata{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _, _ = a.Delete(context.Background(), docID) }()

	// Global resolves.
	if g, err := a.GetByReference(context.Background(), ref); err != nil || g.ID != docID {
		t.Fatalf("global by-reference = (%v, %v), want %v", g.ID, err, docID)
	}
	// Scoped to staging resolves; scoped to conversation misses.
	if s, err := a.GetByReferenceInBucket(context.Background(), ref, stagingBucket); err != nil || s.ID != docID {
		t.Fatalf("scoped(staging) = (%v, %v), want %v", s.ID, err, docID)
	}
	if _, err := a.GetByReferenceInBucket(context.Background(), ref, convBucket); !errors.Is(err, model.ErrDocumentNotFound) {
		t.Fatalf("scoped(conversation) before move err = %v, want ErrDocumentNotFound", err)
	}

	// MOVE: bucket + auth + createdBy + keep ref.
	doc, err := a.GetByID(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	sender := uuid.New()
	if err := a.UpdateMetadata(context.Background(), docID, model.DocumentMetadataUpdate{
		StorageBucketID:   convBucket,
		TemporaryLocation: false,
		DisplayName:       doc.DisplayName,
		AuthorizationID:   &authB,
		CreatedBy:         &sender,
		ExternalReference: &ref,
	}, doc.Version); err != nil {
		t.Fatalf("UpdateMetadata (move): %v", err)
	}

	// After move: conversation resolves, staging misses, global still resolves.
	if s, err := a.GetByReferenceInBucket(context.Background(), ref, convBucket); err != nil || s.ID != docID {
		t.Fatalf("scoped(conversation) after move = (%v, %v), want %v", s.ID, err, docID)
	}
	if _, err := a.GetByReferenceInBucket(context.Background(), ref, stagingBucket); !errors.Is(err, model.ErrDocumentNotFound) {
		t.Fatalf("scoped(staging) after move err = %v, want ErrDocumentNotFound", err)
	}
	moved, err := a.GetByReference(context.Background(), ref)
	if err != nil || moved.ID != docID {
		t.Fatalf("global by-reference after move = (%v, %v), want %v", moved.ID, err, docID)
	}
	if moved.AuthorizationID != authB {
		t.Errorf("authorizationId = %v, want %v (re-attributed)", moved.AuthorizationID, authB)
	}
	if moved.CreatedBy == nil || *moved.CreatedBy != sender {
		t.Errorf("createdBy = %v, want %v", moved.CreatedBy, sender)
	}
}

// T015: two rows sharing one externalID (a re-share copy) keep refcount = 2;
// deleting one keeps the blob referenced (count = 1) — Matrix-aware GC via
// ordinary rows. Both rows carry their own externalReference.
func TestCopy_SharedBlobRefcount(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	bucketA, bucketB := twoBuckets(t, pool)
	authA, cleanupA := newAuthPolicy(t, pool)
	defer cleanupA()
	authB, cleanupB := newAuthPolicy(t, pool)
	defer cleanupB()

	externalID := "shared-blob-" + uuid.New().String()[:12]
	refA := "media-id-A-" + uuid.New().String()
	refB := "media-id-B-" + uuid.New().String()
	now := time.Now()

	idA, _ := uuid.NewV7()
	idB, _ := uuid.NewV7()
	mk := func(id, bucket, auth uuid.UUID, ref string) {
		if _, err := a.Create(context.Background(), model.Document{
			ID:                id,
			ExternalID:        externalID,
			MimeType:          "image/jpeg",
			Size:              10,
			DisplayName:       "shared.jpg",
			StorageBucketID:   bucket,
			AuthorizationID:   auth,
			ExternalReference: &ref,
			CreatedDate:       now,
			UpdatedDate:       now,
		}, model.ContentMetadata{}); err != nil {
			t.Fatalf("Create %v: %v", id, err)
		}
	}
	mk(idA, bucketA, authA, refA)
	defer func() { _, _ = a.Delete(context.Background(), idA) }()
	mk(idB, bucketB, authB, refB)
	defer func() { _, _ = a.Delete(context.Background(), idB) }()

	if count, err := a.CountByExternalID(context.Background(), externalID); err != nil || count != 2 {
		t.Fatalf("refcount = (%d, %v), want 2", count, err)
	}

	// Delete one row → blob still referenced by the other (count = 1).
	if _, err := a.Delete(context.Background(), idB); err != nil {
		t.Fatalf("Delete idB: %v", err)
	}
	if count, err := a.CountByExternalID(context.Background(), externalID); err != nil || count != 1 {
		t.Fatalf("refcount after one delete = (%d, %v), want 1 (blob must survive)", count, err)
	}
}
