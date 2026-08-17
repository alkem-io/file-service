package alkemiodb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestCIDMigrationAdapter_RealPostgresPreservesNonAddressColumns(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := uuid.New()
	bucketID := uuid.New()
	authID := uuid.New()
	cid := "Qmaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = tx.Exec(ctx, `INSERT INTO file
        (id, "externalID", "mimeType", size, "displayName", "temporaryLocation",
         "storageBucketId", "authorizationId", "createdDate", "updatedDate", version, content_metadata)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11)`,
		id, cid, "text/plain", 0, "legacy.txt", true, bucketID, authID, now, 7, []byte(`{"kept":true}`))
	if err != nil {
		t.Fatal(err)
	}

	a := New(tx)
	changed, err := a.UpdateCIDGroup(ctx, target, []string{cid})
	if err != nil || changed != 1 {
		t.Fatalf("UpdateCIDGroup = (%d, %v)", changed, err)
	}
	var gotExternalID, gotMIME, gotName string
	var gotSize, gotVersion int
	var gotTemporary bool
	var gotBucket, gotAuth uuid.UUID
	var gotCreated, gotUpdated time.Time
	var gotMetadata []byte
	err = tx.QueryRow(ctx, `SELECT "externalID", "mimeType", size, "displayName",
        "temporaryLocation", "storageBucketId", "authorizationId", "createdDate",
        "updatedDate", version, content_metadata FROM file WHERE id=$1`, id).
		Scan(&gotExternalID, &gotMIME, &gotSize, &gotName, &gotTemporary, &gotBucket,
			&gotAuth, &gotCreated, &gotUpdated, &gotVersion, &gotMetadata)
	if err != nil {
		t.Fatal(err)
	}
	type persistedRow struct {
		externalID, mime, name string
		size, version          int
		temporary              bool
		bucket, auth           uuid.UUID
		created, updated       time.Time
		metadata               string
	}
	got := persistedRow{gotExternalID, gotMIME, gotName, gotSize, gotVersion, gotTemporary, gotBucket, gotAuth, gotCreated, gotUpdated, string(gotMetadata)}
	want := persistedRow{target, "text/plain", "legacy.txt", 0, 7, true, bucketID, authID, now, now, `{"kept": true}`}
	if got != want {
		t.Fatalf("unexpected persisted row: externalID=%q mime=%q size=%d name=%q temporary=%v bucket=%s auth=%s version=%d created=%s updated=%s metadata=%s",
			gotExternalID, gotMIME, gotSize, gotName, gotTemporary, gotBucket, gotAuth,
			gotVersion, gotCreated, gotUpdated, gotMetadata)
	}
}
