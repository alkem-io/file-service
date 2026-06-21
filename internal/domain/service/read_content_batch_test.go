package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func newBatchService(repo *mockRepo, storage *mockStorage) *FileService {
	return &FileService{
		Repo:      repo,
		Auth:      &mockAuth{allowed: true},
		Storage:   storage,
		Processor: &mockProcessor{},
		Logger:    nopLogger,
	}
}

// ReadContentBatch returns one result per requested id, in request order, each
// carrying the document's blob + MIME — reusing the same GetByID + Storage.Read
// path as the single-document read. This is the reusable core the batched HTTP
// endpoint sits on.
func TestReadContentBatch_ReturnsEachBlobInOrder(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	repo := &mockRepo{docsByID: map[uuid.UUID]model.Document{
		a: {ID: a, ExternalID: "ext-a", MimeType: "text/plain"},
		b: {ID: b, ExternalID: "ext-b", MimeType: "application/octet-stream"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{
		"ext-a": []byte("AAA"),
		"ext-b": []byte("BBB"),
	}}
	svc := newBatchService(repo, storage)

	results := svc.ReadContentBatch(context.Background(), []uuid.UUID{a, b})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].ID != a || !results[0].Found || string(results[0].Content) != "AAA" || results[0].MimeType != "text/plain" {
		t.Errorf("results[0] = %+v, want a/found/AAA/text-plain", results[0])
	}
	if results[1].ID != b || !results[1].Found || string(results[1].Content) != "BBB" || results[1].MimeType != "application/octet-stream" {
		t.Errorf("results[1] = %+v, want b/found/BBB/octet-stream", results[1])
	}
}

// A document id with no row is reported non-fatally (Found=false, Err set) and
// does not abort the batch — the other ids still resolve.
func TestReadContentBatch_UnknownDocumentNonFatal(t *testing.T) {
	present, missing := uuid.New(), uuid.New()
	repo := &mockRepo{docsByID: map[uuid.UUID]model.Document{
		present: {ID: present, ExternalID: "ext", MimeType: "text/plain"},
		// missing intentionally absent.
	}}
	storage := &mockStorage{dataByID: map[string][]byte{"ext": []byte("ok")}}
	svc := newBatchService(repo, storage)

	results := svc.ReadContentBatch(context.Background(), []uuid.UUID{present, missing})

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if !results[0].Found {
		t.Errorf("results[0] (present) Found = false, want true")
	}
	if results[1].Found {
		t.Errorf("results[1] (missing) Found = true, want false")
	}
	if results[1].Err == nil {
		t.Errorf("results[1] (missing) Err = nil, want non-fatal not-found error")
	}
	if !errors.Is(results[1].Err, model.ErrDocumentNotFound) {
		t.Errorf("results[1].Err = %v, want wraps ErrDocumentNotFound", results[1].Err)
	}
}

// A document whose blob is gone from storage is also a non-fatal miss: the row
// resolves but Storage.Read fails, so Found=false with the read error recorded.
func TestReadContentBatch_BlobReadErrorNonFatal(t *testing.T) {
	id := uuid.New()
	repo := &mockRepo{docsByID: map[uuid.UUID]model.Document{
		id: {ID: id, ExternalID: "ext-gone", MimeType: "text/plain"},
	}}
	// dataByID is non-nil but doesn't contain "ext-gone" → Read returns readErr.
	storage := &mockStorage{
		dataByID: map[string][]byte{"other": []byte("x")},
		readErr:  errors.New("blob not on storage"),
	}
	svc := newBatchService(repo, storage)

	results := svc.ReadContentBatch(context.Background(), []uuid.UUID{id})

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Found {
		t.Errorf("Found = true, want false on storage read error")
	}
	if results[0].Err == nil {
		t.Errorf("Err = nil, want the storage read error recorded")
	}
}

// An empty id slice yields an empty (non-nil) result slice — the service layer
// imposes no minimum; the HTTP layer is where "empty request" becomes a 400.
func TestReadContentBatch_EmptyInput(t *testing.T) {
	svc := newBatchService(&mockRepo{}, &mockStorage{})
	results := svc.ReadContentBatch(context.Background(), nil)
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

// Duplicate ids are each resolved positionally — the batch is a list, not a
// set, so the caller's index-based fan-in stays aligned.
func TestReadContentBatch_DuplicatesPreserved(t *testing.T) {
	id := uuid.New()
	repo := &mockRepo{docsByID: map[uuid.UUID]model.Document{
		id: {ID: id, ExternalID: "ext", MimeType: "text/plain"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{"ext": []byte("dup")}}
	svc := newBatchService(repo, storage)

	results := svc.ReadContentBatch(context.Background(), []uuid.UUID{id, id, id})

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	for i, r := range results {
		if !r.Found || string(r.Content) != "dup" {
			t.Errorf("results[%d] = %+v, want found/dup", i, r)
		}
	}
}
