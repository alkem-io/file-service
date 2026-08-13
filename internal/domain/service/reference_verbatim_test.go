package service

import (
	"bytes"
	"context"
	"errors"
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

// THE production re-share path (013): the server COPYs a media_id into a
// conversation bucket with skipDedup=true, and that same (externalReference,
// bucket) pair is already materialized — a repeated / retried re-share. The
// partial UNIQUE(externalReference, storageBucketId) raises a duplicate key, and
// the copy must resolve IDEMPOTENTLY to the existing row (Reused=true), not fail.
//
// skipDedup means "do not CONTENT-dedup"; it never means "do not resolve a
// reference collision". Checking it first made the reference re-query dead code
// on the only path that actually uses it — every real re-share sends
// skipDedup=true — and turned an idempotent retry into a 409.
func TestCopyDocument_ReShareOfExistingReferenceIsIdempotent(t *testing.T) {
	ref := "media_id_reshare"
	destBucket := uuid.New()
	source := model.Document{ID: uuid.New(), ExternalID: "hash", MimeType: "image/png", Size: 5, DisplayName: "m.png"}
	existing := model.Document{
		ID: uuid.New(), ExternalID: "hash", MimeType: "image/png", Size: 5,
		StorageBucketID: destBucket, ExternalReference: &ref,
	}
	repo := &mockRepo{doc: source, createErr: dupOnReference(), refDoc: &existing}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	doc, err := svc.CopyDocument(context.Background(), source.ID, model.CopyDocumentInput{
		DestinationBucketID: destBucket,
		AuthorizationID:     uuid.New(),
		ExternalReference:   &ref,
		SkipDedup:           true, // what the real re-share always sends
	})
	if err != nil {
		t.Fatalf("re-share of an already-copied reference must resolve idempotently, got %v", err)
	}
	if doc.ID != existing.ID || !doc.Reused {
		t.Errorf("resolved id=%v reused=%v, want the existing row %v with Reused=true", doc.ID, doc.Reused, existing.ID)
	}
	if repo.refCalls != 1 || repo.lastRefKey != ref {
		t.Errorf("collision was not resolved BY REFERENCE: refCalls=%d key=%q", repo.refCalls, repo.lastRefKey)
	}
	if repo.findCalls != 0 {
		t.Errorf("reference collision consulted content-dedup (findCalls=%d), want 0", repo.findCalls)
	}
}

// The counterpart the fix must not swallow: a skipDedup collision on any index
// OTHER than the reference one is still a hard 409. skipDedup asked for a fresh
// row and the schema refused; masquerading that as a dedup hit would silently
// corrupt placeholder flows.
func TestInsertDocument_SkipDedupWithoutReferenceStillConflicts(t *testing.T) {
	repo := &mockRepo{createErr: dupOnOther(), findDoc: &model.Document{ID: uuid.New()}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	_, err := svc.CreateDocument(context.Background(), model.CreateDocumentInput{
		DisplayName:     "p.bin",
		StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(),
		SkipDedup:       true,
	}, []byte("payload"), "", nil, 0)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("skipDedup content collision = %v, want ErrConflict", err)
	}
}

// A REFERENCE-BEARING insert whose violation came from some OTHER index (the
// authorizationId unique, say) must NOT be resolved as a reference collision:
// the reference lookup is not even consulted, and a skipDedup caller gets its
// 409. The row carrying this reference may well exist in another bucket, and
// probing for it here is how the wrong row gets returned as "the winner".
func TestInsertDocument_NonReferenceIndexIsNotResolvedByReference(t *testing.T) {
	ref := "media_id_absent"
	// Scripted so a reference re-query would SUCCEED — the test can only pass
	// because the classification stopped it from running.
	repo := &mockRepo{createErr: dupOnOther(), refDoc: &model.Document{ID: uuid.New(), ExternalID: "other-hash"}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	_, err := svc.CreateDocument(context.Background(), model.CreateDocumentInput{
		DisplayName:       "m.bin",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
		SkipDedup:         true,
	}, []byte("payload"), "", nil, 0)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict for a non-reference index violation under skipDedup", err)
	}
	if repo.refCalls != 0 {
		t.Errorf("a non-reference violation was probed by reference (refCalls=%d)", repo.refCalls)
	}
}

// THE B2 case: the violation IS the reference index, but the row that owned
// (reference, bucket) was deleted between the failed insert and the re-query.
// There is nothing to resolve to — and nothing to fall through to, because the
// content lookup filters `externalReference IS NULL` and in a reference-only
// bucket can only answer with an UNRELATED reference-less row. The race must
// surface as ErrConflict (a retryable 409), never as that unrelated row with
// Reused=true and never as a "source document not found" 404.
func TestInsertDocument_ReferenceWinnerVanishedIsConflictNotAWrongRow(t *testing.T) {
	ref := "media_id_deleted_mid_race"
	unrelated := model.Document{ID: uuid.New(), ExternalID: "hash", DisplayName: "someone-elses-row"}
	// refDoc nil → the reference re-query misses; findDoc set → a fall-through
	// to content dedup would hand back this unrelated reference-less row.
	repo := &mockRepo{createErr: dupOnReference(), findDoc: &unrelated}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	doc, err := svc.CreateDocument(context.Background(), model.CreateDocumentInput{
		DisplayName:       "m.bin",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
	}, []byte("payload"), "", nil, 0)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when the reference winner vanished mid-race", err)
	}
	if doc != nil {
		t.Errorf("returned document %+v; a vanished winner must resolve to no row at all", doc)
	}
	if repo.findCalls != 0 {
		t.Errorf("a vanished reference winner fell through to content dedup (findCalls=%d) and would return an unrelated row", repo.findCalls)
	}
	if !errors.Is(err, ErrConflict) || errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("err = %v, must not read as a not-found (the handler would render a misleading 404)", err)
	}
}

// A unique violation the database did not attribute to any index is
// UNRESOLVABLE: the two resolutions are mutually exclusive and each is wrong for
// the other index. It must fail loudly — never a silent dedup hit, never a 404.
func TestInsertDocument_UnattributableViolationFailsLoudly(t *testing.T) {
	ref := "media_id_x"
	repo := &mockRepo{
		createErr: dupUnattributed(), // a unique violation the database did not name
		refDoc:    &model.Document{ID: uuid.New(), ExternalID: "ref-hash"},
		findDoc:   &model.Document{ID: uuid.New(), ExternalID: "content-hash"},
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}, Processor: &mockProcessor{}}

	doc, err := svc.CreateDocument(context.Background(), model.CreateDocumentInput{
		DisplayName:       "m.bin",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: &ref,
	}, []byte("payload"), "", nil, 0)
	if err == nil {
		t.Fatalf("unattributable unique violation resolved to %+v; want an error", doc)
	}
	if errors.Is(err, model.ErrDocumentNotFound) {
		t.Errorf("err = %v, must not read as a not-found (the handler would render a misleading 404)", err)
	}
	if repo.refCalls != 0 || repo.findCalls != 0 {
		t.Errorf("an unattributable violation was probed anyway: refCalls=%d findCalls=%d", repo.refCalls, repo.findCalls)
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
		createErr: dupOnReference(),
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
