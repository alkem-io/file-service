package service

import (
	"context"
	"errors"
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

// A panic while measuring one row (this decodes arbitrary legacy bytes through a CGo image library,
// in a bare background goroutine) must be contained to that row — not take down the process at boot,
// which would crash-loop on a poison blob. The row is counted skipped and the sweep continues.
func TestRunDimsBackfill_PanicOnOneRow_IsContained(t *testing.T) {
	repo := &mockRepo{imagesNeedingDims: oneImageRow()}
	svc := newDimsSvc(repo, &mockStorage{data: []byte("poison")}, &mockProcessor{measurePanic: true})

	var sum DimsBackfillSummary
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("panic escaped the sweep (would crash the process at boot): %v", rec)
			}
		}()
		sum = svc.RunDimsBackfill(context.Background())
	}()
	if sum.Skipped != 1 || repo.backfillCalls != 0 {
		t.Fatalf("summary=%+v backfillCalls=%d, want the poison row skipped with no persist", sum, repo.backfillCalls)
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
