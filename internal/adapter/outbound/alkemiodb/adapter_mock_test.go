package alkemiodb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func columns() []string {
	return []string{"id", "externalID", "mimeType", "size", "displayName", "createdBy",
		"temporaryLocation", "storageBucketId", "authorizationId", "tagsetId",
		"createdDate", "updatedDate", "version", "content_metadata", "externalReference"}
}

func TestMock_GetByID_Found(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	docID := uuid.New()
	authID := uuid.New()
	bucketID := uuid.New()
	now := time.Now()

	mock.ExpectQuery("SELECT .+ FROM file WHERE id").
		WithArgs(pgtype.UUID{Bytes: docID, Valid: true}).
		WillReturnRows(mock.NewRows(columns()).AddRow(
			pgtype.UUID{Bytes: docID, Valid: true},
			"abc123",
			"text/plain",
			int32(42),
			"test.txt",
			pgtype.UUID{Valid: false},
			false,
			pgtype.UUID{Bytes: bucketID, Valid: true},
			pgtype.UUID{Bytes: authID, Valid: true},
			pgtype.UUID{Valid: false},
			pgtype.Timestamptz{Time: now, Valid: true},
			pgtype.Timestamptz{Time: now, Valid: true},
			int32(1),
			[]byte("{}"),
			pgtype.Text{Valid: false}, // externalReference
		))

	a := New(mock)
	doc, err := a.GetByID(context.Background(), docID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if doc.ID != docID {
		t.Errorf("ID = %v, want %v", doc.ID, docID)
	}
	if doc.ExternalID != "abc123" {
		t.Errorf("ExternalID = %q", doc.ExternalID)
	}
	if doc.MimeType != "text/plain" {
		t.Errorf("MimeType = %q", doc.MimeType)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMock_GetByID_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT .+ FROM file WHERE id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows(columns()))

	a := New(mock)
	_, err = a.GetByID(context.Background(), uuid.New())
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMock_Create_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	docID := uuid.New()

	mock.ExpectQuery("INSERT INTO file").
		WithArgs(
			pgxmock.AnyArg(), // id
			pgxmock.AnyArg(), // externalID
			pgxmock.AnyArg(), // mimeType
			pgxmock.AnyArg(), // size
			pgxmock.AnyArg(), // displayName
			pgxmock.AnyArg(), // createdBy
			pgxmock.AnyArg(), // temporaryLocation
			pgxmock.AnyArg(), // storageBucketId
			pgxmock.AnyArg(), // authorizationId
			pgxmock.AnyArg(), // tagsetId
			pgxmock.AnyArg(), // createdDate
			pgxmock.AnyArg(), // updatedDate
			[]byte(`{}`),     // content_metadata: empty Populated=false → "{}"
			pgxmock.AnyArg(), // externalReference
		).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(pgtype.UUID{Bytes: docID, Valid: true}))

	a := New(mock)
	now := time.Now()
	id, err := a.Create(context.Background(), model.Document{
		ID:              docID,
		ExternalID:      "hash",
		MimeType:        "text/plain",
		Size:            10,
		DisplayName:     "test.txt",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		CreatedDate:     now,
		UpdatedDate:     now,
	}, model.ContentMetadata{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != docID {
		t.Errorf("ID = %v, want %v", id, docID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The zero-UUID → SQL NULL mapping for authorizationId is the SINGLE mechanism
// that lets every provider staging store coexist under UNIQUE("authorizationId"):
// a document created with no server-minted authorization must write NULL, not
// the all-zero UUID, which would be a real value and collide across rows (and
// FK-violate against a policy that does not exist).
//
// Pin the actual pgtype value at the insert boundary — Valid:false vs a
// zero-bytes UUID with Valid:true — because that distinction is invisible
// everywhere above the adapter (both read back as uuid.Nil).
func TestMock_Create_AuthorizationIDNullMapping(t *testing.T) {
	authID := uuid.New()
	for _, tc := range []struct {
		name string
		auth uuid.UUID
		want pgtype.UUID
	}{
		{"minted policy writes the value", authID, pgtype.UUID{Bytes: authID, Valid: true}},
		{"no policy writes SQL NULL", uuid.Nil, pgtype.UUID{Valid: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			docID := uuid.New()
			// Two identical inserts: for the NULL case this is the coexistence
			// property itself — two policy-less staging rows both write NULL, so
			// neither can collide with the other on the nullable unique index.
			// (pgxmock cannot enforce the index; what is asserted here is the
			// value the adapter sends, which is what makes coexistence possible.)
			for range 2 {
				mock.ExpectQuery("INSERT INTO file").
					WithArgs(
						pgxmock.AnyArg(), // id
						pgxmock.AnyArg(), // externalID
						pgxmock.AnyArg(), // mimeType
						pgxmock.AnyArg(), // size
						pgxmock.AnyArg(), // displayName
						pgxmock.AnyArg(), // createdBy
						pgxmock.AnyArg(), // temporaryLocation
						pgxmock.AnyArg(), // storageBucketId
						tc.want,          // authorizationId — the assertion
						pgxmock.AnyArg(), // tagsetId
						pgxmock.AnyArg(), // createdDate
						pgxmock.AnyArg(), // updatedDate
						[]byte(`{}`),     // content_metadata
						pgxmock.AnyArg(), // externalReference
					).
					WillReturnRows(mock.NewRows([]string{"id"}).AddRow(pgtype.UUID{Bytes: docID, Valid: true}))
			}

			a := New(mock)
			now := time.Now()
			for i := range 2 {
				ref := "media_id_" + string(rune('a'+i))
				if _, err := a.Create(context.Background(), model.Document{
					ID:                uuid.New(),
					ExternalID:        "hash",
					MimeType:          "image/png",
					DisplayName:       "m.png",
					StorageBucketID:   uuid.New(),
					AuthorizationID:   tc.auth,
					ExternalReference: &ref,
					CreatedDate:       now,
					UpdatedDate:       now,
				}, model.ContentMetadata{}); err != nil {
					t.Fatalf("Create #%d: %v", i, err)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestMock_UpdateFile_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	// Param order: id, new externalID, MIME, size, updatedDate, metadata,
	// expected old externalID, expected version.
	// Content metadata is the empty-Populated case → marshals to "{}".
	mock.ExpectExec("UPDATE file SET").
		WithArgs(pgxmock.AnyArg(), "newhash", "image/jpeg", pgxmock.AnyArg(), pgxmock.AnyArg(), []byte(`{}`), "oldhash", int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	a := New(mock)
	err = a.UpdateFile(context.Background(), uuid.New(), "oldhash", 1, "newhash", "image/jpeg", 999, model.ContentMetadata{})
	if err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMock_UpdateFile_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectExec("UPDATE file SET").
		WithArgs(pgxmock.AnyArg(), "hash", "text/plain", pgxmock.AnyArg(), pgxmock.AnyArg(), []byte(`{}`), "oldhash", int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	a := New(mock)
	err = a.UpdateFile(context.Background(), uuid.New(), "oldhash", 1, "hash", "text/plain", 1, model.ContentMetadata{})
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestMock_Delete_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	authID := uuid.New()
	tagsetID := uuid.New()

	mock.ExpectQuery("DELETE FROM file WHERE id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"externalID", "authorizationId", "tagsetId"}).
			AddRow("abc123", pgtype.UUID{Bytes: authID, Valid: true}, pgtype.UUID{Bytes: tagsetID, Valid: true}))

	a := New(mock)
	deleted, err := a.Delete(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.AuthorizationID != authID {
		t.Errorf("AuthorizationID = %v, want %v", deleted.AuthorizationID, authID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMock_Delete_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("DELETE FROM file WHERE id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(mock.NewRows([]string{"externalID", "authorizationId", "tagsetId"}))

	a := New(mock)
	_, err = a.Delete(context.Background(), uuid.New())
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestMock_CountByExternalID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT COUNT").
		WithArgs("somehash").
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(int64(3)))

	a := New(mock)
	count, err := a.CountByExternalID(context.Background(), "somehash")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestMock_UpdateMetadata_Success(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	// Param order in UpdateDocumentMetadata (move + re-attribute): $1=id,
	// $2=storageBucketId, $3=temporaryLocation, $4=displayName, $5=authorizationId,
	// $6=createdBy, $7=externalReference, $8=updatedDate, $9=version. Pin
	// everything except updatedDate (timestamp computed in adapter).
	docID := uuid.New()
	bucketID := uuid.New()
	authID := uuid.New()
	ref := "media_id_abc"
	mock.ExpectExec("UPDATE file SET").
		WithArgs(uuidToPgx(docID), uuidToPgx(bucketID), false, "name.txt",
			uuidToPgxNullable(&authID), pgtype.UUID{Valid: false}, stringToPgxText(&ref),
			pgxmock.AnyArg(), int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	a := New(mock)
	err = a.UpdateMetadata(context.Background(), docID, model.DocumentMetadataUpdate{
		StorageBucketID:   bucketID,
		TemporaryLocation: false,
		DisplayName:       "name.txt",
		AuthorizationID:   &authID,
		ExternalReference: &ref,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMock_UpdateMetadata_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectExec("UPDATE file SET").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	a := New(mock)
	err = a.UpdateMetadata(context.Background(), uuid.New(), model.DocumentMetadataUpdate{
		StorageBucketID: uuid.New(),
		DisplayName:     "name.txt",
	}, 1)
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("expected ErrDocumentNotFound, got %v", err)
	}
}

func TestMock_GetByID_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT .+ FROM file WHERE id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection reset"))

	a := New(mock)
	_, err = a.GetByID(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, model.ErrDocumentNotFound) {
		t.Error("should not be ErrDocumentNotFound for connection error")
	}
}

func TestMock_UpdateFile_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectExec("UPDATE file SET").
		WithArgs(pgxmock.AnyArg(), "h", "t", pgxmock.AnyArg(), pgxmock.AnyArg(), []byte(`{}`), "old", int32(1)).
		WillReturnError(errors.New("connection reset"))

	a := New(mock)
	err = a.UpdateFile(context.Background(), uuid.New(), "old", 1, "h", "t", 1, model.ContentMetadata{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMock_UpdateMetadata_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectExec("UPDATE file SET").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("connection reset"))

	a := New(mock)
	err = a.UpdateMetadata(context.Background(), uuid.New(), model.DocumentMetadataUpdate{
		StorageBucketID: uuid.New(),
		DisplayName:     "name.txt",
	}, 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMock_Delete_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("DELETE FROM file WHERE id").
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection reset"))

	a := New(mock)
	_, err = a.Delete(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, model.ErrDocumentNotFound) {
		t.Error("should not be ErrDocumentNotFound for connection error")
	}
}

func TestMock_CountByExternalID_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("SELECT COUNT").
		WithArgs("hash").
		WillReturnError(errors.New("connection reset"))

	a := New(mock)
	_, err = a.CountByExternalID(context.Background(), "hash")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMock_Create_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery("INSERT INTO file").
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			[]byte(`{}`), // content_metadata: empty Populated=false → "{}"
		).
		WillReturnError(errors.New("FK constraint violation"))

	a := New(mock)
	_, err = a.Create(context.Background(), model.Document{
		ID:              uuid.New(),
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		CreatedDate:     time.Now(),
		UpdatedDate:     time.Now(),
	}, model.ContentMetadata{})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestMock_ListImagesNeedingDims_PredicateGuardsSentinels: the "never re-decode a row we already
// decided about" invariant lives ONLY in this query's WHERE clause — the domain sweep uses a mock
// repo that bypasses it. So assert the predicate itself: image rows, content_metadata still EMPTY
// ('{}'), keyset-paged by id. Broadening it (e.g. to rows merely missing imageWidth) would re-decode
// every {_decodeFailed:true} sentinel on every boot — a CPU/IO storm this test exists to prevent.
func TestMock_ListImagesNeedingDims_PredicateGuardsSentinels(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	after := uuid.New()

	mock.ExpectQuery(`(?s)FROM file\s+WHERE "mimeType" LIKE 'image/%'\s+AND content_metadata = '\{\}'::jsonb\s+AND id > \$1\s+ORDER BY id\s+LIMIT \$2`).
		WithArgs(pgtype.UUID{Bytes: after, Valid: true}, int32(500)).
		WillReturnRows(mock.NewRows([]string{"id", "externalID", "mimeType"}))

	if _, err := New(mock).ListImagesNeedingDims(context.Background(), after, 500); err != nil {
		t.Fatalf("ListImagesNeedingDims: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestMock_FindByExternalIDAndBucket_ExcludesReferenceRows: the dual-identity
// rule's keystone is a single predicate — `AND "externalReference" IS NULL` in
// FindDocumentByExternalIDAndBucket — and it lives ONLY in that SQL. Every layer
// above is blind to it: the domain tests use a mock repo that never sees a WHERE
// clause, and the adapter's other tests match the statement with a loose
// `SELECT .+ FROM file` regex that a dropped predicate sails straight through.
//
// Without it, content-dedup would match a REFERENCE-bearing row: a plain upload
// of bytes that happen to equal an inbound Matrix attachment would dedup onto
// that bridge row and couple its lifecycle to it, and two distinct media_ids
// with identical bytes could collapse into one document — the exact confusion
// the reference/content split exists to prevent. So assert the SQL itself.
func TestMock_FindByExternalIDAndBucket_ExcludesReferenceRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	bucketID := uuid.New()
	mock.ExpectQuery(`(?s)FROM file\s+WHERE "externalID" = \$1 AND "storageBucketId" = \$2 AND "externalReference" IS NULL\s+ORDER BY "createdDate" ASC, id ASC\s+LIMIT 1`).
		WithArgs("hash-abc", pgtype.UUID{Bytes: bucketID, Valid: true}).
		WillReturnRows(mock.NewRows(columns()))

	_, err = New(mock).FindByExternalIDAndBucket(context.Background(), "hash-abc", bucketID)
	if !errors.Is(err, model.ErrDocumentNotFound) {
		t.Fatalf("FindByExternalIDAndBucket = %v, want ErrDocumentNotFound for an empty result", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// The reference-keyed lookups are the other half of the dual identity: they must
// stay keyed on "externalReference" (global and bucket-scoped), with the
// deterministic oldest-wins ordering the by-reference contract promises.
func TestMock_ReferenceLookups_KeyOnExternalReference(t *testing.T) {
	bucketID := uuid.New()

	t.Run("global", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		mock.ExpectQuery(`(?s)FROM file\s+WHERE "externalReference" = \$1\s+ORDER BY "createdDate" ASC, id ASC\s+LIMIT 1`).
			WithArgs(pgtype.Text{String: "media_id_x", Valid: true}).
			WillReturnRows(mock.NewRows(columns()))

		if _, err := New(mock).GetByReference(context.Background(), "media_id_x"); !errors.Is(err, model.ErrDocumentNotFound) {
			t.Fatalf("GetByReference = %v, want ErrDocumentNotFound", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("bucket-scoped", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		mock.ExpectQuery(`(?s)FROM file\s+WHERE "externalReference" = \$1 AND "storageBucketId" = \$2\s+ORDER BY "createdDate" ASC, id ASC\s+LIMIT 1`).
			WithArgs(pgtype.Text{String: "media_id_x", Valid: true}, pgtype.UUID{Bytes: bucketID, Valid: true}).
			WillReturnRows(mock.NewRows(columns()))

		if _, err := New(mock).GetByReferenceInBucket(context.Background(), "media_id_x", bucketID); !errors.Is(err, model.ErrDocumentNotFound) {
			t.Fatalf("GetByReferenceInBucket = %v, want ErrDocumentNotFound", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

// TestMock_GetByIDs_OneRoundTripAndPartialResult pins the two properties that only exist
// at the SQL boundary and are invisible everywhere above it (the HTTP tests use a mock repo
// that never issues a statement):
//
//  1. ONE round-trip. The whole point of POST /internal/file/meta-batch is collapsing a
//     caller's N-per-attachment fan-out into a single query. pgxmock is ordered and strict,
//     so an adapter that looped GetDocumentByID would issue a statement that matches no
//     expectation and fail here — and nowhere else.
//  2. Unresolved ids are simply ABSENT. Three ids are requested, two rows come back; that is
//     a normal success, not an error, because a row may be deleted between the caller's read
//     and this batch.
//
// The second returned row is POLICY-LESS (NULL authorizationId / createdBy / tagsetId) to
// prove the batch row goes through the SAME documentFromRow conversion as every other read —
// a NULL id must land as uuid.Nil/nil so the handler omits it rather than publishing an
// all-zero policy.
func TestMock_GetByIDs_OneRoundTripAndPartialResult(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	found, policyless, missing := uuid.New(), uuid.New(), uuid.New()
	bucketID, authID := uuid.New(), uuid.New()
	now := time.Now()

	mock.ExpectQuery(`(?s)FROM file\s+WHERE id = ANY\(\$1::uuid\[\]\)`).
		WithArgs([]pgtype.UUID{
			{Bytes: found, Valid: true},
			{Bytes: policyless, Valid: true},
			{Bytes: missing, Valid: true},
		}).
		WillReturnRows(mock.NewRows(columns()).
			AddRow(
				pgtype.UUID{Bytes: found, Valid: true},
				"hash_found",
				"image/png",
				int32(42),
				"found.png",
				pgtype.UUID{Valid: false},
				false,
				pgtype.UUID{Bytes: bucketID, Valid: true},
				pgtype.UUID{Bytes: authID, Valid: true},
				pgtype.UUID{Valid: false},
				pgtype.Timestamptz{Time: now, Valid: true},
				pgtype.Timestamptz{Time: now, Valid: true},
				int32(1),
				[]byte(`{"imageWidth":800,"imageHeight":600}`),
				pgtype.Text{Valid: false},
			).
			AddRow(
				pgtype.UUID{Bytes: policyless, Valid: true},
				"hash_staging",
				"image/jpeg",
				int32(7),
				"staging.jpg",
				pgtype.UUID{Valid: false}, // createdBy NULL
				true,
				pgtype.UUID{Bytes: bucketID, Valid: true},
				pgtype.UUID{Valid: false}, // authorizationId NULL — the matrix_media staging store
				pgtype.UUID{Valid: false}, // tagsetId NULL
				pgtype.Timestamptz{Time: now, Valid: true},
				pgtype.Timestamptz{Time: now, Valid: true},
				int32(1),
				[]byte("{}"),
				pgtype.Text{String: "media_abc", Valid: true},
			))

	docs, err := New(mock).GetByIDs(context.Background(), []uuid.UUID{found, policyless, missing})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2 (the id with no row is omitted, not an error)", len(docs))
	}
	if docs[0].ID != found || docs[1].ID != policyless {
		t.Errorf("ids = %v, %v; want %v, %v", docs[0].ID, docs[1].ID, found, policyless)
	}
	assertBatchDims(t, docs[0], 800, 600)
	assertPolicylessBatchRow(t, docs[1])
	// Strict + ordered: this fails if the adapter issued any statement other than the single
	// batch query, or issued it more than once.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// assertBatchDims checks that a batch row's content_metadata JSONB was parsed — the batch
// must not be a thinner read than GetByID.
func assertBatchDims(t *testing.T, doc model.Document, wantW, wantH int) {
	t.Helper()
	if doc.ImageWidth == nil || doc.ImageHeight == nil {
		t.Fatalf("dims = %v x %v, want %d x %d — content_metadata must be parsed on the batch row too",
			doc.ImageWidth, doc.ImageHeight, wantW, wantH)
	}
	if *doc.ImageWidth != wantW || *doc.ImageHeight != wantH {
		t.Errorf("dims = %d x %d, want %d x %d", *doc.ImageWidth, *doc.ImageHeight, wantW, wantH)
	}
}

// assertPolicylessBatchRow checks the NULL-column half of the shared conversion: a staging
// row's NULL ids must land as uuid.Nil / nil so the handler OMITS them, while its non-NULL
// externalReference still comes through.
func assertPolicylessBatchRow(t *testing.T, doc model.Document) {
	t.Helper()
	if doc.AuthorizationID != uuid.Nil {
		t.Errorf("AuthorizationID = %v, want uuid.Nil for a NULL column", doc.AuthorizationID)
	}
	if doc.CreatedBy != nil {
		t.Errorf("CreatedBy = %v, want nil for a NULL column", doc.CreatedBy)
	}
	if doc.TagsetID != nil {
		t.Errorf("TagsetID = %v, want nil for a NULL column", doc.TagsetID)
	}
	if doc.ExternalReference == nil {
		t.Fatal("ExternalReference = nil, want media_abc")
	}
	if *doc.ExternalReference != "media_abc" {
		t.Errorf("ExternalReference = %q, want media_abc", *doc.ExternalReference)
	}
}
