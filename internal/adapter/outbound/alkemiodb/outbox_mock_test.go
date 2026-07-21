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
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(13)...).
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
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(13)...).
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
	mock.ExpectQuery("INSERT INTO file").WithArgs(anyArgs(13)...).
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

// TestMock_UpdateFileWithOutbox_Commits: a content replace updates the row and enqueues the new
// hash in one transaction, then NOTIFYs.
func TestMock_UpdateFileWithOutbox_Commits(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE file").WithArgs(anyArgs(6)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("INSERT INTO file_backup_outbox").
		WithArgs(pgtype.UUID{Bytes: id, Valid: true}, "hashNew", int16(0),
			pgxmock.AnyArg(), pgxmock.AnyArg(), int64(20)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectExec("NOTIFY file_backup_outbox").WillReturnResult(pgxmock.NewResult("NOTIFY", 0))

	err = New(mock).UpdateFileWithOutbox(context.Background(), id, "hashNew", "image/jpeg", 20, model.ContentMetadata{}, 0)
	if err != nil {
		t.Fatalf("UpdateFileWithOutbox: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_UpdateFileWithOutbox_NotFoundRollsBack: 0 rows updated → ErrDocumentNotFound, the tx
// rolls back and no outbox row is enqueued.
func TestMock_UpdateFileWithOutbox_NotFoundRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE file").WithArgs(anyArgs(6)...).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err = New(mock).UpdateFileWithOutbox(context.Background(), uuid.New(), "h", "text/plain", 1, model.ContentMetadata{}, 0)
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
	mock.ExpectExec("UPDATE file").WithArgs(anyArgs(6)...).
		WillReturnError(&pgconn.PgError{Code: pgerrcode.UniqueViolation})
	mock.ExpectRollback()

	err = New(mock).UpdateFileWithOutbox(context.Background(), uuid.New(), "h", "image/jpeg", 5, model.ContentMetadata{}, 0)
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

// TestMock_DeletePendingForFile_ScopedAndGuarded: the orphan-hygiene delete must carry BOTH the
// outer scope (fileId + externalID + status='pending') AND the NOT EXISTS guard — either half
// dropped is a real regression (drop the scope → a whole-table mass over-delete; drop the guard →
// reopen the same-doc A→B→A live-hint deletion). pgxmock does unanchored substring matching, so
// the regex asserts the FULL statement structure (both halves) — a subtly-wrong query fails, not
// only a total removal of the clause.
func TestMock_DeletePendingForFile_ScopedAndGuarded(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	fileID := uuid.New()

	mock.ExpectExec(`(?s)DELETE FROM file_backup_outbox o\s+WHERE o\."fileId" = \$1 AND o\."externalID" = \$2 AND o\.status = 'pending'\s+AND NOT EXISTS \(SELECT 1 FROM file f WHERE f\.id = \$1 AND f\."externalID" = \$2\)`).
		WithArgs(pgtype.UUID{Bytes: fileID, Valid: true}, "somehash").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	n, err := New(mock).DeletePendingForFile(context.Background(), fileID, "somehash")
	if err != nil || n != 1 {
		t.Fatalf("DeletePendingForFile = %d, %v (want 1, nil)", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
