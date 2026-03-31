package alkemiodb

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/alkem-io/file-service-go/internal/domain/model"
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
	rows, err := pool.Query(context.Background(), `SELECT id FROM document LIMIT 1`)
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
		`SELECT "storageBucketId", "authorizationId" FROM document WHERE "storageBucketId" IS NOT NULL AND "authorizationId" IS NOT NULL LIMIT 1`,
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

	createdID, err := a.Create(context.Background(), doc)
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
	var origExtID string
	err := pool.QueryRow(context.Background(),
		`SELECT id, "externalID" FROM document LIMIT 1`,
	).Scan(&id, &origExtID)
	if err != nil {
		t.Skip("no documents")
	}
	docID := uuid.UUID(id)

	newExtID := "updated-" + uuid.New().String()[:8]
	err = a.UpdateFile(context.Background(), docID, newExtID, "text/plain", 99)
	if err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}

	// Restore original
	defer func() {
		_ = a.UpdateFile(context.Background(), docID, origExtID, "image/png", 0)
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

	err := a.UpdateFile(context.Background(), uuid.New(), "hash", "text/plain", 1)
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
		`SELECT "externalID" FROM document LIMIT 1`,
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

func TestUpdateLocation(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	var id, bucketID [16]byte
	var tempLoc bool
	err := pool.QueryRow(context.Background(),
		`SELECT id, "storageBucketId", "temporaryLocation" FROM document LIMIT 1`,
	).Scan(&id, &bucketID, &tempLoc)
	if err != nil {
		t.Skip("no documents")
	}
	docID := uuid.UUID(id)
	origBucket := uuid.UUID(bucketID)

	// Update and restore
	err = a.UpdateLocation(context.Background(), docID, origBucket, !tempLoc)
	if err != nil {
		t.Fatalf("UpdateLocation: %v", err)
	}
	defer func() {
		_ = a.UpdateLocation(context.Background(), docID, origBucket, tempLoc)
	}()

	doc, _ := a.GetByID(context.Background(), docID)
	if doc.TemporaryLocation != !tempLoc {
		t.Errorf("temporaryLocation = %v, want %v", doc.TemporaryLocation, !tempLoc)
	}
}

func TestUpdateLocation_NotFound(t *testing.T) {
	pool := testPool(t)
	defer pool.Close()
	a := New(pool)

	err := a.UpdateLocation(context.Background(), uuid.New(), uuid.New(), false)
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
