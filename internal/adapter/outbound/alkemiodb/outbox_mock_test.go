package alkemiodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func anyArgs(n int) []any {
	a := make([]any, n)
	for i := range a {
		a[i] = pgxmock.AnyArg()
	}
	return a
}

func sampleDoc(id uuid.UUID) model.Document {
	bucket, authz := uuid.New(), uuid.New()
	return model.Document{
		ID: id, ExternalID: "hashX", MimeType: "application/x-yjs", Size: 10,
		DisplayName: "wb.yjs", StorageBucketID: bucket, AuthorizationID: authz,
		CreatedDate: time.Now(), UpdatedDate: time.Now(),
	}
}

// TestMock_CreateWithOutbox_Commits: the document insert and the outbox enqueue commit in ONE
// transaction, then a NOTIFY fires. Asserts the outbox row carries the object's hash + priority.
func TestMock_CreateWithOutbox_Commits(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	docID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(14)...).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(pgtype.UUID{Bytes: docID, Valid: true}))
	mock.ExpectExec("INSERT INTO file_backup_outbox").
		WithArgs(pgtype.UUID{Bytes: docID, Valid: true}, "hashX", int16(1),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(10)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnResult(pgxmock.NewResult("NOTIFY", 0))

	id, err := New(mock).CreateWithOutbox(context.Background(), sampleDoc(docID), model.ContentMetadata{}, 1)
	if err != nil {
		t.Fatalf("CreateWithOutbox: %v", err)
	}
	if id != docID {
		t.Fatalf("id = %v, want %v", id, docID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_CreateWithOutbox_DedupRollsBack: a unique violation on the document rolls the
// transaction back (NO outbox row, NO NOTIFY) and surfaces model.ErrDuplicateKey so the
// service's dedup path re-queries the winner.
func TestMock_CreateWithOutbox_DedupRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	docID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(14)...).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()

	_, err = New(mock).CreateWithOutbox(context.Background(), sampleDoc(docID), model.ContentMetadata{}, 0)
	if !errors.Is(err, model.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_CreateWithOutbox_NotifyFailureNonFatal: the post-commit NOTIFY is best-effort — if it
// fails, the create still succeeds (the row is committed; the consumer's poll floor drains it).
func TestMock_CreateWithOutbox_NotifyFailureNonFatal(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	docID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(14)...).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(pgtype.UUID{Bytes: docID, Valid: true}))
	mock.ExpectExec("INSERT INTO file_backup_outbox").WithArgs(anyArgs(6)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnError(errors.New("notify boom"))

	id, err := New(mock).CreateWithOutbox(context.Background(), sampleDoc(docID), model.ContentMetadata{}, 1)
	if err != nil {
		t.Fatalf("a failed NOTIFY must not fail the committed create, got: %v", err)
	}
	if id != docID {
		t.Fatalf("id = %v, want %v", id, docID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateFileWithOutbox_DurableEnqueues: a content replace on a DURABLE row (the UPDATE's
// RETURNING "temporaryLocation" is false) updates the row and enqueues the new hash in one
// transaction, then NOTIFYs. The UPDATE is now :one (a Query), and the in-tx RETURNING flag — not
// the caller's pre-loaded snapshot — is what gates the enqueue. Reports enqueued=true.
func TestMock_UpdateFileWithOutbox_DurableEnqueues(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(6)...).
		WillReturnRows(mock.NewRows([]string{"temporaryLocation"}).AddRow(false))
	mock.ExpectExec("INSERT INTO file_backup_outbox").
		WithArgs(pgtype.UUID{Bytes: id, Valid: true}, "hashNew", int16(0),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(20)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnResult(pgxmock.NewResult("NOTIFY", 0))

	enqueued, err := New(mock).UpdateFileWithOutbox(context.Background(), id, "hashNew", "image/jpeg", 20, model.ContentMetadata{}, 0)
	if err != nil {
		t.Fatalf("UpdateFileWithOutbox: %v", err)
	}
	if !enqueued {
		t.Fatal("a durable replace must report enqueued=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateFileWithOutbox_StillTemporary_NoEnqueue: a content replace whose in-tx RETURNING
// "temporaryLocation" is true (still staging) commits the content UPDATE but enqueues NO outbox row
// — a still-temporary doc's content isn't backed up yet; its later temporary→durable flip enqueues
// the then-current hash. Reports enqueued=false. This is the guard for a replace that raced BEHIND
// a concurrent temp→durable flip: the decision comes from the locked row, so a stale "was temporary"
// snapshot can no longer strand the (still-temporary) content or double-enqueue it.
func TestMock_UpdateFileWithOutbox_StillTemporary_NoEnqueue(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(6)...).
		WillReturnRows(mock.NewRows([]string{"temporaryLocation"}).AddRow(true))
	// No INSERT INTO file_backup_outbox — the enqueue closure is a no-op on a still-temporary row.
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnResult(pgxmock.NewResult("NOTIFY", 0))

	enqueued, err := New(mock).UpdateFileWithOutbox(context.Background(), id, "hashNew", "image/jpeg", 20, model.ContentMetadata{}, 0)
	if err != nil {
		t.Fatalf("UpdateFileWithOutbox: %v", err)
	}
	if enqueued {
		t.Fatal("a still-temporary replace must report enqueued=false (no backup row)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateFileWithOutbox_NotFoundRollsBack: 0 rows updated (RETURNING yields no row →
// pgx.ErrNoRows) → ErrDocumentNotFound, the tx rolls back and no outbox row is enqueued.
func TestMock_UpdateFileWithOutbox_NotFoundRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(6)...).
		WillReturnRows(mock.NewRows([]string{"temporaryLocation"}))
	mock.ExpectRollback()

	_, err = New(mock).UpdateFileWithOutbox(context.Background(), uuid.New(), "h", "text/plain", 1, model.ContentMetadata{}, 0)
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Fatalf("want ErrDocumentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateFileWithOutbox_DuplicateRollsBack: a unique violation on the content update rolls
// the tx back (no outbox row, no NOTIFY) and surfaces model.ErrDuplicateKey, matching UpdateFile.
func TestMock_UpdateFileWithOutbox_DuplicateRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(6)...).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()

	_, err = New(mock).UpdateFileWithOutbox(context.Background(), uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{}, 0)
	if !errors.Is(err, model.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateMetadataWithOutbox_Commits: a temporary→durable PATCH applies the versioned
// metadata update and enqueues the now-durable object's backup hint in ONE transaction, then
// NOTIFYs. The versioned UPDATE RETURNs the row's AUTHORITATIVE externalID/size (read while the row
// is UPDATE-locked in this tx); the enqueue MUST carry those RETURNING values — there is no
// caller-passed externalID/size anymore. This is the stale-breadcrumb-race guard: a concurrent
// content-replace that swapped externalID/size (without bumping version) is reflected here because
// the hint is sourced from the locked row, not a handler snapshot. Also asserts the method returns
// the same authoritative values for the reload-free PATCH response.
func TestMock_UpdateMetadataWithOutbox_Commits(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id := uuid.New()

	now := time.Now()
	mock.ExpectBegin()
	// The UPDATE is now :one RETURNING the FULL row — a Query, not an Exec. The returned hash/size
	// (values a concurrent content-replace could have swapped in) drive BOTH the enqueue and the
	// returned document, proving the outbox never carries a stale handler-threaded breadcrumb and the
	// PATCH response is built from the same authoritative row.
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(9)...).
		WillReturnRows(mock.NewRows(columns()).AddRow(
			uuidToPgx(id),
			"hashDur",
			"application/x-yjs",
			int32(99),
			"wb.yjs",
			pgtype.UUID{Valid: false},
			false,
			pgtype.UUID{Valid: false},
			pgtype.UUID{Valid: false},
			pgtype.UUID{Valid: false},
			pgtype.Timestamptz{Time: now, Valid: true},
			pgtype.Timestamptz{Time: now, Valid: true},
			int32(2),
			[]byte("{}"),
			pgtype.Text{Valid: false},
		))
	mock.ExpectExec("INSERT INTO file_backup_outbox").
		WithArgs(pgtype.UUID{Bytes: id, Valid: true}, "hashDur", int16(1),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(99)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnResult(pgxmock.NewResult("NOTIFY", 0))

	// priorityFor is fed the AUTHORITATIVE post-update mime (the RETURNING row's "application/x-yjs"),
	// not a caller snapshot — returning 1 only for that mime proves the enqueued int16(1) came from
	// the locked row's mime.
	priorityFor := func(mimeType string) int16 {
		if mimeType == "application/x-yjs" {
			return 1
		}
		return 0
	}
	doc, err := New(mock).UpdateMetadataWithOutbox(context.Background(), id, model.DocumentMetadataUpdate{}, 1, priorityFor)
	if err != nil {
		t.Fatalf("UpdateMetadataWithOutbox: %v", err)
	}
	if doc.ExternalID != "hashDur" || doc.Size != 99 {
		t.Fatalf("returned %q/%d, want the RETURNING authoritative externalID/size hashDur/99", doc.ExternalID, doc.Size)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateMetadataWithOutbox_NotFoundRollsBack: a version mismatch / missing row (RETURNING
// yields no row → pgx.ErrNoRows) → ErrDocumentNotFound, the tx rolls back and no outbox row is
// enqueued.
func TestMock_UpdateMetadataWithOutbox_NotFoundRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(9)...).
		WillReturnRows(mock.NewRows(columns()))
	mock.ExpectRollback()

	// The tx rolls back before the enqueue closure, so priorityFor is never invoked.
	_, err = New(mock).UpdateMetadataWithOutbox(context.Background(), uuid.New(), model.DocumentMetadataUpdate{}, 1, func(string) int16 { return 0 })
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Fatalf("want ErrDocumentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateMetadataWithOutbox_DuplicateRollsBack: a unique violation on the metadata update
// (a re-homed reference / re-attributed authorizationId collision) rolls the tx back (no outbox
// row, no NOTIFY) and surfaces model.ErrDuplicateKey, matching UpdateMetadata.
func TestMock_UpdateMetadataWithOutbox_DuplicateRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE file").WithArgs(anyArgs(9)...).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()

	// The tx rolls back before the enqueue closure, so priorityFor is never invoked.
	_, err = New(mock).UpdateMetadataWithOutbox(context.Background(), uuid.New(), model.DocumentMetadataUpdate{}, 1, func(string) int16 { return 0 })
	if !errors.Is(err, model.ErrDuplicateKey) {
		t.Fatalf("want ErrDuplicateKey, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_PruneBackupOutbox: prunes done rows older than the cutoff and returns the count.
func TestMock_PruneBackupOutbox(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	mock.ExpectExec("DELETE FROM file_backup_outbox").
		WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("DELETE", 3))
	n, err := New(mock).PruneBackupOutbox(context.Background(), time.Now())
	if err != nil || n != 3 {
		t.Fatalf("PruneBackupOutbox = %d, %v (want 3, nil)", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
