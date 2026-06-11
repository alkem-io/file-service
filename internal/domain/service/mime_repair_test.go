package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/alkem-io/file-service-go/internal/domain/model"
)

// Spec 019 / FR-006 — boot-time MIME repair.

func TestRunMimeRepair_RelabelsVerifiedOfficeZip(t *testing.T) {
	docID := uuid.New()
	repo := &mockRepo{suspects: []model.Document{
		{ID: docID, ExternalID: "zipdoc", MimeType: "application/zip", Size: 1234, DisplayName: "Deck.pptx"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{
		"zipdoc": append([]byte("PK\x03\x04"), make([]byte, 64)...),
	}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	sum := svc.RunMimeRepair(context.Background())

	if sum.Relabeled != 1 || sum.Unrecoverable != 0 || sum.SkippedNotOffice != 0 || sum.Errors != 0 {
		t.Fatalf("summary = %+v, want exactly 1 relabeled", sum)
	}
	if got := repo.relabeled[docID]; got != pptxMime {
		t.Errorf("relabeled to %q, want %q", got, pptxMime)
	}
	if got := repo.lastListMimeTypes; len(got) != 3 {
		t.Errorf("suspect scan used %v, want the 3 generic MIME types", got)
	}
}

func TestRunMimeRepair_ZeroByteIsUnrecoverable(t *testing.T) {
	docID := uuid.New()
	repo := &mockRepo{suspects: []model.Document{
		{ID: docID, ExternalID: "emptyhash", MimeType: "text/plain", Size: 0, DisplayName: "New Presentation.pptx"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{"emptyhash": {}}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	sum := svc.RunMimeRepair(context.Background())

	if sum.Unrecoverable != 1 || sum.Relabeled != 0 {
		t.Fatalf("summary = %+v, want exactly 1 unrecoverable", sum)
	}
	if len(repo.relabeled) != 0 {
		t.Error("zero-byte row was relabeled; relabeling an empty blob creates an openable-but-blank lie")
	}
}

func TestRunMimeRepair_NonZipOfficeNameSkipped(t *testing.T) {
	docID := uuid.New()
	repo := &mockRepo{suspects: []model.Document{
		{ID: docID, ExternalID: "texty", MimeType: "text/plain", Size: 11, DisplayName: "notes.docx"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{"texty": []byte("just text\n")}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	sum := svc.RunMimeRepair(context.Background())

	if sum.SkippedNotOffice != 1 || sum.Relabeled != 0 || sum.Unrecoverable != 0 {
		t.Fatalf("summary = %+v, want exactly 1 skipped", sum)
	}
	if len(repo.relabeled) != 0 {
		t.Error("non-zip content was relabeled to an office type")
	}
}

func TestRunMimeRepair_NonOfficeNamesAreNotSuspects(t *testing.T) {
	repo := &mockRepo{suspects: []model.Document{
		{ID: uuid.New(), ExternalID: "a", MimeType: "text/plain", DisplayName: "readme.txt"},
		{ID: uuid.New(), ExternalID: "b", MimeType: "application/zip", DisplayName: "archive.zip"},
	}}
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	sum := svc.RunMimeRepair(context.Background())

	if sum != (MimeRepairSummary{}) {
		t.Fatalf("summary = %+v, want all zeros (legitimately generic rows untouched)", sum)
	}
}

func TestRunMimeRepair_Idempotent(t *testing.T) {
	docID := uuid.New()
	zipContent := append([]byte("PK\x03\x04"), make([]byte, 16)...)
	repo := &mockRepo{suspects: []model.Document{
		{ID: docID, ExternalID: "zipdoc", MimeType: "application/zip", DisplayName: "Deck.pptx"},
	}}
	storage := &mockStorage{dataByID: map[string][]byte{"zipdoc": zipContent}}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	first := svc.RunMimeRepair(context.Background())
	if first.Relabeled != 1 {
		t.Fatalf("first run: %+v, want 1 relabeled", first)
	}

	// After a successful relabel the row no longer matches the suspect
	// predicate — the second scan returns nothing.
	repo.suspects = nil
	second := svc.RunMimeRepair(context.Background())
	if second != (MimeRepairSummary{}) {
		t.Fatalf("second run: %+v, want all zeros (idempotency)", second)
	}
}

func TestRunMimeRepair_StorageErrorCountedJobContinues(t *testing.T) {
	okID := uuid.New()
	repo := &mockRepo{suspects: []model.Document{
		{ID: uuid.New(), ExternalID: "missing", MimeType: "application/zip", DisplayName: "lost.pptx"},
		{ID: okID, ExternalID: "zipdoc", MimeType: "application/zip", DisplayName: "Deck.pptx"},
	}}
	storage := &mockStorage{
		dataByID: map[string][]byte{"zipdoc": append([]byte("PK\x03\x04"), make([]byte, 16)...)},
		readErr:  errors.New("blob gone"),
	}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage}

	sum := svc.RunMimeRepair(context.Background())

	if sum.Errors != 1 || sum.Relabeled != 1 {
		t.Fatalf("summary = %+v, want 1 error and 1 relabeled (job continues past errors)", sum)
	}
	if got := repo.relabeled[okID]; got != pptxMime {
		t.Errorf("healthy row not relabeled after earlier error: %q", got)
	}
}

func TestRunMimeRepair_ScanFailureReported(t *testing.T) {
	repo := &mockRepo{listErr: errors.New("db down")}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: &mockStorage{}}

	sum := svc.RunMimeRepair(context.Background())
	if sum.Errors != 1 {
		t.Fatalf("summary = %+v, want 1 error", sum)
	}
}
