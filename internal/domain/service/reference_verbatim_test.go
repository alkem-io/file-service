package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// Verbatim store (013 skipImageProcessing): a transcodable image stored with
// skipImageProcessing=true takes the pass-through arm — no transcode, bytes
// byte-identical — while dimensions are still measured non-destructively (a
// header-only read that does not touch the stored bytes), so the conversation
// attachment still gets its dims.
func TestStageUpload_VerbatimSkipsTranscodeButMeasuresDims(t *testing.T) {
	w, h := 320, 240
	raw := []byte("jpegish raw bytes")

	t.Run("skip=true stores raw and does not transcode", func(t *testing.T) {
		storage := &mockStorage{}
		proc := &mockProcessor{detectMIME: "image/jpeg", measureDimsW: &w, measureDimsH: &h}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}

		su, err := svc.StageUpload(context.Background(), bytes.NewReader(raw), "", true)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		if proc.transcodeCalls != 0 {
			t.Errorf("verbatim store routed through the transcoder (transcodeCalls=%d), want 0", proc.transcodeCalls)
		}
		if su.MimeType != "image/jpeg" {
			t.Errorf("MimeType = %q, want image/jpeg (unchanged by verbatim store)", su.MimeType)
		}
		if len(storage.stages) != 1 || !bytes.Equal(storage.stages[0].buf.Bytes(), raw) {
			t.Errorf("stored bytes not byte-identical to input under verbatim store")
		}
		if proc.measureDimsCalls != 1 || su.ImageWidth == nil || *su.ImageWidth != w {
			t.Errorf("verbatim image dims not measured non-destructively: calls=%d width=%v", proc.measureDimsCalls, su.ImageWidth)
		}
		su.Discard()
	})

	t.Run("skip=false transcodes (control)", func(t *testing.T) {
		storage := &mockStorage{}
		proc := &mockProcessor{detectMIME: "image/jpeg"}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: proc}

		su, err := svc.StageUpload(context.Background(), bytes.NewReader(raw), "", false)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		if proc.transcodeCalls != 1 {
			t.Errorf("non-verbatim transcodable image transcodeCalls = %d, want 1", proc.transcodeCalls)
		}
		su.Discard()
	})
}

// Reference-keyed dedup (013): a reference-bearing create is identity'd by its
// externalReference, so it bypasses per-bucket content-dedup entirely — two
// media_ids with identical bytes must stay distinct rows. The dedup lookup
// (FindByExternalIDAndBucket) must never be consulted, even when a matching
// content row exists.
func TestCompleteUpload_ReferenceBearingSkipsContentDedup(t *testing.T) {
	ref := "media_id_xyz"
	dedupHit := model.Document{ID: uuid.New(), ExternalID: "same"}
	repo := &mockRepo{findDoc: &dedupHit} // would dedup if content-dedup were consulted
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:       "m.bin",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("payload"), "", nil, 0)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if repo.findCalls != 0 {
		t.Errorf("reference-bearing create consulted content-dedup (findCalls=%d), want 0", repo.findCalls)
	}
	if repo.createCalls != 1 {
		t.Errorf("reference-bearing create did not insert a fresh row (createCalls=%d)", repo.createCalls)
	}
	if doc.Reused {
		t.Errorf("reference-bearing create returned Reused=true, want a fresh row")
	}
	if repo.lastCreateDoc.ExternalReference == nil || *repo.lastCreateDoc.ExternalReference != ref {
		t.Errorf("externalReference not persisted on the new row: %v", repo.lastCreateDoc.ExternalReference)
	}
}

// A NON-reference (plain) create still dedups normally — the reference-keyed
// bypass must not disable content-dedup for plain uploads.
func TestCompleteUpload_PlainCreateStillDedups(t *testing.T) {
	dedupHit := model.Document{ID: uuid.New(), ExternalID: "same"}
	repo := &mockRepo{findDoc: &dedupHit}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{DisplayName: "p.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("payload"), "", nil, 0)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if repo.findCalls != 1 {
		t.Errorf("plain create must consult content-dedup once, got findCalls=%d", repo.findCalls)
	}
	if !doc.Reused {
		t.Errorf("plain create over existing content must dedup (Reused=true)")
	}
}

// A reference-bearing copy likewise bypasses content-dedup and materializes a
// fresh row carrying the caller's externalReference.
func TestCopyDocument_ReferenceBearingSkipsContentDedup(t *testing.T) {
	ref := "media_id_reshare"
	source := model.Document{ID: uuid.New(), ExternalID: "hash", MimeType: "image/png", Size: 5}
	// findDoc set to prove the reference path never consults content-dedup.
	dedupHit := model.Document{ID: uuid.New(), ExternalID: "hash"}
	repo := &mockRepo{doc: source, findDoc: &dedupHit}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	doc, err := svc.CopyDocument(context.Background(), source.ID, model.CopyDocumentInput{
		DestinationBucketID: uuid.New(),
		AuthorizationID:     uuid.New(),
		ExternalReference:   &ref,
	})
	if err != nil {
		t.Fatalf("CopyDocument: %v", err)
	}
	if repo.findCalls != 0 {
		t.Errorf("reference-bearing copy consulted content-dedup (findCalls=%d), want 0", repo.findCalls)
	}
	if doc.Reused {
		t.Errorf("reference-bearing copy returned Reused=true, want a fresh row")
	}
	if repo.lastCreateDoc.ExternalReference == nil || *repo.lastCreateDoc.ExternalReference != ref {
		t.Errorf("externalReference not carried onto the copied row: %v", repo.lastCreateDoc.ExternalReference)
	}
}

// On a unique-violation race, a reference-bearing insert re-resolves the winner
// by reference (GetByReferenceInBucket), NOT by content — the reference is the
// row's identity.
func TestInsertDocument_ReferenceRaceReQueriesByReference(t *testing.T) {
	ref := "media_id_race"
	winner := model.Document{ID: uuid.New(), ExternalID: "hash"}
	repo := &mockRepoRace{
		find:      func() (model.Document, error) { return model.Document{}, model.ErrDocumentNotFound },
		createErr: model.ErrDuplicateKey,
		refWinner: &winner,
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	input := model.CreateDocumentInput{
		DisplayName:       "m.bin",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
	}
	doc, err := svc.CreateDocument(context.Background(), input, []byte("payload"), "", nil, 0)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if doc.ID != winner.ID || !doc.Reused {
		t.Errorf("reference race did not resolve the winner by reference: got id=%v reused=%v", doc.ID, doc.Reused)
	}
}
