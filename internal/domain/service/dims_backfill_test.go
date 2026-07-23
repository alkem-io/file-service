package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func newDimsSvc(repo *mockRepo, storage *mockStorage, proc *mockProcessor) *FileService {
	return &FileService{Repo: repo, Storage: storage, Processor: proc, Logger: nopLogger}
}

func oneImageRow() []model.Document {
	return []model.Document{{ID: uuid.New(), ExternalID: "img1", MimeType: "image/jpeg"}}
}

// A successful measure persists the dims via the compare-and-set and counts measured.
func TestRunDimsBackfill_Measured(t *testing.T) {
	w, h := 800, 600
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("jpegbytes")}, &mockProcessor{measureDimsW: &w, measureDimsH: &h})

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Measured != 1 || sum.DecodeFailed != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want measured=1 only", sum)
	}
	if repo.backfillCalls != 1 {
		t.Fatalf("backfillCalls = %d, want 1", repo.backfillCalls)
	}
	p := repo.lastBackfillPayload
	if !p.Populated || p.ImageWidth == nil || *p.ImageWidth != 800 || p.ImageHeight == nil || *p.ImageHeight != 600 {
		t.Fatalf("persisted payload = %+v, want populated dims 800x600", p)
	}
}

// A decoder error persists the {_decodeFailed:true} sentinel so the row is never retried.
func TestRunDimsBackfill_DecodeFailed_PersistsSentinel(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("corrupt")}, &mockProcessor{measureDimsErr: errors.New("vips load failed")})

	sum := svc.RunDimsBackfill(context.Background())
	if sum.DecodeFailed != 1 || sum.Measured != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want decodeFailed=1 only", sum)
	}
	if p := repo.lastBackfillPayload; !p.Populated || !p.DecodeFailed || p.ImageWidth != nil {
		t.Fatalf("payload = %+v, want the {_decodeFailed:true} sentinel", p)
	}
}

// (nil, nil, nil) — the no-vips stub reports "no decoder": skip the persist so a future vips-capable
// run retries the still-empty row (does NOT poison it with a sentinel).
func TestRunDimsBackfill_NoDecoder_SkipsPersist(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{}) // MeasureDims → (nil,nil,nil)

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Skipped != 1 || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want skipped=1 with NO persist", sum, repo.backfillCalls)
	}
}

// A storage read failure leaves the row empty for a future pass — no decode, no persist.
func TestRunDimsBackfill_StorageReadFails_SkipsPersist(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{readErr: errors.New("gone")}, &mockProcessor{})

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Skipped != 1 || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want skipped=1 with NO persist", sum, repo.backfillCalls)
	}
}

// A panic while measuring one row (this decodes arbitrary legacy bytes through a CGo image library)
// must be contained to that row — not take down the whole sweep process on a poison blob. The row is
// counted skipped and the sweep continues.
func TestRunDimsBackfill_PanicOnOneRow_IsContained(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("poison")}, &mockProcessor{measurePanic: true})

	var sum DimsBackfillSummary
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("panic escaped the sweep (would crash the sweep process): %v", rec)
			}
		}()
		sum = svc.RunDimsBackfill(context.Background())
	}()
	if sum.Skipped != 1 || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want the poison row skipped with no persist", sum, repo.backfillCalls)
	}
}

// [7] The compare-and-set must be keyed on the externalID the sweep MEASURED — that is what stops a
// concurrent content replace from having the old blob's dims written over its new content.
func TestRunDimsBackfill_PersistsCASOnMeasuredExternalID(t *testing.T) {
	w, h := 10, 20
	rows := oneImageRow()
	repo := &mockRepo{imagesNeedingDims: rows}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{measureDimsW: &w, measureDimsH: &h})

	svc.RunDimsBackfill(context.Background())
	if repo.lastBackfillExternalID != rows[0].ExternalID {
		t.Fatalf("CAS externalID = %q, want the measured row's %q", repo.lastBackfillExternalID, rows[0].ExternalID)
	}
	if repo.lastBackfillID != rows[0].ID {
		t.Fatalf("CAS id = %v, want %v", repo.lastBackfillID, rows[0].ID)
	}
}

// [8] A poison row must not stop the sweep: with a panicking row BETWEEN two good ones, both good
// rows are still measured (the cursor advances past the failure rather than wedging on it).
func TestRunDimsBackfill_ContinuesPastPoisonRow(t *testing.T) {
	w, h := 4, 5
	rows := []model.Document{
		{ID: uuid.New(), ExternalID: "good1", MimeType: "image/jpeg"},
		{ID: uuid.New(), ExternalID: "poison", MimeType: "image/jpeg"},
		{ID: uuid.New(), ExternalID: "good2", MimeType: "image/jpeg"},
	}
	repo := &mockRepo{imagesNeedingDims: rows}
	proc := &mockProcessor{measureDimsW: &w, measureDimsH: &h, panicOnContent: []byte{2}}
	svc := newDimsSvc(repo, &mockStorage{dataByID: map[string][]byte{"good1": {1}, "poison": {2}, "good2": {3}}}, proc)

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Measured != 2 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v, want measured=2 skipped=1 (swept past the poison row)", sum)
	}
}

// [9] A persist failure must NOT be counted as a success — the row stays unpopulated for a future
// run, so it has to land in Skipped (both the measured and the sentinel branch).
func TestRunDimsBackfill_PersistFailure_CountsSkipped(t *testing.T) {
	w, h := 1, 2
	t.Run("measured-persist-fails", func(t *testing.T) {
		repo := &mockRepo{imagesNeedingDims: oneImageRow(), backfillErr: errors.New("db down")}
		svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{measureDimsW: &w, measureDimsH: &h})
		if sum := svc.RunDimsBackfill(context.Background()); sum.Skipped != 1 || sum.Measured != 0 {
			t.Fatalf("summary = %+v, want skipped=1 measured=0", sum)
		}
	})
	t.Run("sentinel-persist-fails", func(t *testing.T) {
		repo := &mockRepo{imagesNeedingDims: oneImageRow(), backfillErr: errors.New("db down")}
		svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{measureDimsErr: errors.New("corrupt")})
		if sum := svc.RunDimsBackfill(context.Background()); sum.Skipped != 1 || sum.DecodeFailed != 0 {
			t.Fatalf("summary = %+v, want skipped=1 decodeFailed=0", sum)
		}
	})
}

func TestRunDimsBackfill_CASLost_CountsSkipped(t *testing.T) {
	w, h := 1, 2
	t.Run("measured", func(t *testing.T) {
		repo := &mockRepo{imagesNeedingDims: oneImageRow(), backfillLostRace: true}
		svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{measureDimsW: &w, measureDimsH: &h})
		if sum := svc.RunDimsBackfill(context.Background()); sum.Skipped != 1 || sum.Measured != 0 {
			t.Fatalf("summary = %+v, want skipped=1 measured=0", sum)
		}
	})
	t.Run("sentinel", func(t *testing.T) {
		repo := &mockRepo{imagesNeedingDims: oneImageRow(), backfillLostRace: true}
		svc := newDimsSvc(repo, &mockStorage{data: []byte("x")}, &mockProcessor{measureDimsErr: errors.New("corrupt")})
		if sum := svc.RunDimsBackfill(context.Background()); sum.Skipped != 1 || sum.DecodeFailed != 0 {
			t.Fatalf("summary = %+v, want skipped=1 decodeFailed=0", sum)
		}
	})
}

func TestRunDimsBackfill_CancelMidPage_StopsPromptly(t *testing.T) {
	w, h := 1, 2
	rows := []model.Document{
		{ID: uuid.New(), ExternalID: "first", MimeType: "image/jpeg"},
		{ID: uuid.New(), ExternalID: "second", MimeType: "image/jpeg"},
		{ID: uuid.New(), ExternalID: "third", MimeType: "image/jpeg"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	repo := &mockRepo{imagesNeedingDims: rows}
	proc := &mockProcessor{measureDimsW: &w, measureDimsH: &h, measureDimsHook: cancel}
	svc := newDimsSvc(repo, &mockStorage{dataByID: map[string][]byte{
		"first": {1}, "second": {2}, "third": {3},
	}}, proc)

	sum := svc.RunDimsBackfill(ctx)
	if !sum.Aborted || sum.Measured != 1 || repo.backfillCalls != 1 {
		t.Fatalf("summary=%+v backfillCalls=%d, want aborted after exactly one row", sum, repo.backfillCalls)
	}
}

// [0] A TRANSPORT fault mid-header must NOT be mistaken for an undecodable image: writing the
// permanent {_decodeFailed:true} sentinel would exclude a perfectly good image from this and every
// future sweep. It must be a skip, with nothing persisted.
func TestRunDimsBackfill_ReadFaultMidHeader_IsSkippedNotSentinel(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	// The reader fails mid-stream; the decoder surfaces that as a plain load error.
	storage := &mockStorage{streamBody: &faultyReadCloser{}, streamSize: 999}
	svc := newDimsSvc(repo, storage, &mockProcessor{measureDimsErr: errors.New("vips load: streaming load")})

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Skipped != 1 || sum.DecodeFailed != 0 {
		t.Fatalf("summary = %+v, want skipped=1 decodeFailed=0 (I/O fault, not an undecodable image)", sum)
	}
	if repo.backfillCalls != 0 {
		t.Fatalf("backfillCalls = %d, want 0 — a read fault must never persist the permanent sentinel", repo.backfillCalls)
	}
}

// A clean EOF before the size reported by the opened handle is also a storage fault (for example,
// a backing-file truncation race), not evidence that the original immutable image is corrupt.
func TestRunDimsBackfill_ShortReadMidHeader_IsSkippedNotSentinel(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	storage := &mockStorage{
		streamBody: io.NopCloser(bytes.NewReader([]byte("short"))),
		streamSize: 999,
	}
	svc := newDimsSvc(repo, storage, &mockProcessor{measureDimsErr: errors.New("vips load: unexpected EOF")})

	sum := svc.RunDimsBackfill(context.Background())
	if sum.Skipped != 1 || sum.DecodeFailed != 0 || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want short storage read skipped without sentinel", sum, repo.backfillCalls)
	}
}

// An empty legacy set is a clean no-op — the converged steady state, one page scan and done.
func TestRunDimsBackfill_EmptyCorpus_NoOp(t *testing.T) {
	repo := &mockRepo{}
	svc := newDimsSvc(repo, &mockStorage{}, &mockProcessor{})
	if sum := svc.RunDimsBackfill(context.Background()); sum != (DimsBackfillSummary{}) || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want zero no-op", sum, repo.backfillCalls)
	}
}
