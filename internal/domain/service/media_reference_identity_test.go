package service

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func TestMediaReferenceIdentity_NewMediaIDsWithSameBytesStayDistinct(t *testing.T) {
	repo := &mockRepo{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}
	bucket := uuid.New()
	payload := []byte("same physical bytes, different Matrix uploads")
	refs := []string{"first_matrix_media_id", "second_matrix_media_id"}
	documents := make([]model.Document, 0, len(refs))
	for _, ref := range refs {
		doc, err := svc.CreateDocument(context.Background(), model.CreateDocumentInput{
			DisplayName: "attachment.bin", StorageBucketID: bucket,
			ExternalReference: &ref, SkipImageProcessing: true,
		}, payload, "application/octet-stream", nil, 0)
		if err != nil {
			t.Fatalf("create %q: %v", ref, err)
		}
		if doc.Reused || doc.ExternalReference == nil || *doc.ExternalReference != ref {
			t.Fatalf("create %q did not retain its own logical identity: %+v", ref, doc)
		}
		documents = append(documents, *doc)
		// A content lookup during the second upload would incorrectly reuse the
		// first upload. Reference-bearing creates must bypass that lookup.
		repo.findDoc = doc
	}
	first, second := documents[0], documents[1]
	if first.ID == second.ID || first.ID == uuid.Nil || second.ID == uuid.Nil {
		t.Fatalf("distinct media IDs must create distinct file IDs: %v, %v", first.ID, second.ID)
	}
	if first.ExternalID == "" || first.ExternalID != second.ExternalID || first.Size != len(payload) || second.Size != len(payload) {
		t.Fatalf("identical bytes must retain one content identity: first=%+v second=%+v", first, second)
	}
	if repo.createCalls != 2 || repo.findCalls != 0 {
		t.Fatalf("want two logical inserts and no content-row dedup, got inserts=%d lookups=%d", repo.createCalls, repo.findCalls)
	}
}

func TestMediaReferenceIdentity_CopyRetryKeepsFirstPlacementMetadata(t *testing.T) {
	ref := "shared_matrix_media_id"
	w, h := 640, 480
	source := model.Document{
		ID: uuid.New(), ExternalID: "shared-content-hash", ExternalReference: &ref,
		MimeType: "image/png", Size: 512, DisplayName: "provider-name",
		StorageBucketID: uuid.New(),
		ContentMetadata: model.ContentMetadata{Populated: true, ImageWidth: &w, ImageHeight: &h},
		ImageWidth:      &w, ImageHeight: &h,
	}
	repo := &mockRepo{doc: source}
	// COPY is metadata-only: these paths must not need a storage reader or an
	// image processor, even when the caller retries with different metadata.
	svc := &FileService{Logger: nopLogger, Repo: repo}
	firstActor, firstTagset, firstAuth := uuid.New(), uuid.New(), uuid.New()
	firstName := "first résumé.png"
	input := model.CopyDocumentInput{
		DestinationBucketID: uuid.New(), AuthorizationID: firstAuth,
		CreatedBy: &firstActor, TagsetID: &firstTagset, DisplayName: &firstName,
		ExternalReference: &ref, SkipDedup: true,
	}
	first, err := svc.CopyDocument(context.Background(), source.ID, input)
	if err != nil {
		t.Fatalf("first placement: %v", err)
	}
	if first.Reused || first.ID == source.ID || first.ExternalID != source.ExternalID {
		t.Fatalf("first COPY must create a new logical row on the same content: %+v", first)
	}
	if first.AuthorizationID != firstAuth || !reflect.DeepEqual(first.CreatedBy, &firstActor) ||
		!reflect.DeepEqual(first.TagsetID, &firstTagset) || first.DisplayName != firstName {
		t.Fatalf("first placement metadata was not stored: %+v", first)
	}
	// Script the repository's unique-key response. Actual PostgreSQL constraint
	// and concurrent-CAS coverage belongs to the separate integration fixture.
	repo.createErr = dupOnReference()
	repo.refDoc = first
	secondActor, secondTagset := uuid.New(), uuid.New()
	secondName := "later name.png"
	input.AuthorizationID = uuid.New()
	input.CreatedBy, input.TagsetID, input.DisplayName = &secondActor, &secondTagset, &secondName
	retried, err := svc.CopyDocument(context.Background(), source.ID, input)
	if err != nil {
		t.Fatalf("duplicate placement: %v", err)
	}
	want := *first
	want.Reused = true
	if !reflect.DeepEqual(*retried, want) {
		t.Fatalf("retry must return the entire first row unchanged except Reused: got=%+v want=%+v", retried, want)
	}
	if repo.updateMetaCalls != 0 || repo.updateFileCalls != 0 || !reflect.DeepEqual(repo.doc, source) {
		t.Fatal("COPY retry rewrote an existing row or its content")
	}
}

type identityMoveRepo struct {
	mockRepo
	updatedID      uuid.UUID
	updatedVersion int
}

func (r *identityMoveRepo) UpdateMetadata(ctx context.Context, id uuid.UUID, meta model.DocumentMetadataUpdate, version int) error {
	r.updatedID, r.updatedVersion = id, version
	return r.mockRepo.UpdateMetadata(ctx, id, meta, version)
}

func TestMediaReferenceIdentity_FirstMoveKeepsFileIDReferenceAndContent(t *testing.T) {
	ref := "staged_matrix_media_id"
	w, h := 800, 600
	staged := model.Document{
		ID: uuid.New(), ExternalID: "original-content-hash", ExternalReference: &ref,
		StorageBucketID: uuid.New(), MimeType: "image/png", Size: 2048,
		DisplayName: ref, Version: 3, CreatedDate: time.Unix(1_700_000_000, 0),
		ContentMetadata: model.ContentMetadata{Populated: true, ImageWidth: &w, ImageHeight: &h},
		ImageWidth:      &w, ImageHeight: &h,
	}
	actor, auth, tagset := uuid.New(), uuid.New(), uuid.New()
	meta := model.DocumentMetadataUpdate{
		StorageBucketID: uuid.New(), AuthorizationID: &auth, TagsetID: &tagset,
		CreatedBy: &actor, DisplayName: "placed résumé.png", ExternalReference: &ref,
	}
	placed := staged
	placed.StorageBucketID, placed.AuthorizationID, placed.TagsetID = meta.StorageBucketID, auth, &tagset
	placed.CreatedBy, placed.DisplayName, placed.Version = &actor, meta.DisplayName, staged.Version+1
	repo := &identityMoveRepo{mockRepo: mockRepo{docsByID: map[uuid.UUID]model.Document{staged.ID: placed}}}
	svc := &FileService{Logger: nopLogger, Repo: repo}
	got, err := svc.UpdateDocumentMetadata(context.Background(), staged, meta)
	if err != nil {
		t.Fatalf("first placement MOVE: %v", err)
	}
	if repo.updatedID != staged.ID || repo.updatedVersion != staged.Version || repo.updateMetaCalls != 1 {
		t.Fatalf("MOVE must update the original ID with its read version: id=%v version=%d calls=%d", repo.updatedID, repo.updatedVersion, repo.updateMetaCalls)
	}
	if !reflect.DeepEqual(repo.lastUpdateMeta, meta) || !reflect.DeepEqual(*got, placed) {
		t.Fatalf("MOVE must attach all placement metadata and return the same logical file: got=%+v want=%+v", got, placed)
	}
	if repo.createCalls != 0 || repo.updateFileCalls != 0 {
		t.Fatalf("MOVE inserted or replaced content: creates=%d replacements=%d", repo.createCalls, repo.updateFileCalls)
	}
}
