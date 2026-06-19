package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// Spec 019 — replace-path MIME reconciliation (US1/US2/US3).

const (
	pptxMime = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	docxMime = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxMime = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// buildReorderedOOXMLZip emulates a Collabora save-back that defeats OOXML
// signature detection: the detector reads only the first ~3 KiB and needs a
// "ppt/" (or word/, xl/) entry inside that window. Collabora-written
// packages can start with a legitimate entry (e.g. a stored docProps
// thumbnail) large enough to push every office marker past the window, so
// the sniff degrades to application/zip. The fixture reproduces exactly
// that: an allowed first entry with ~8 KiB of stored (incompressible-style)
// payload, office entries after it.
func buildReorderedOOXMLZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	hdr := &zip.FileHeader{Name: "docProps/thumbnail.jpeg", Method: zip.Store}
	f, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	padding := bytes.Repeat([]byte{0xAA, 0x55}, 4096) // 8 KiB, contains no PK marker
	if _, err := f.Write(padding); err != nil {
		t.Fatal(err)
	}

	ct, err := zw.Create("[Content_Types].xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Write([]byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		s, err := zw.Create(fmt.Sprintf("ppt/slides/slide%d.xml", i+1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write([]byte(`<?xml version="1.0"?><p:sld/>`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Premise check against the real detector: the reordered OOXML zip must sniff
// as a generic type, not as an office format. If this ever fails, the
// fallback path would be dead code for this input — re-examine the fix.
func TestReorderedOOXMLZip_SniffsGeneric(t *testing.T) {
	content := buildReorderedOOXMLZip(t)
	detected := mimetype.Detect(content).String()
	if !model.IsGenericMIME(detected) {
		t.Fatalf("reordered OOXML zip sniffed as %q; expected a generic type — premise of the fallback fix", detected)
	}
}

func TestReconcileReplaceMIME_Matrix(t *testing.T) {
	content := []byte("non-empty body") // sniff value is driven by the mock, not the bytes
	cases := []struct {
		name        string
		known       string
		sniff       string
		content     []byte
		wantMime    string
		wantOutcome string
		wantErr     error
	}{
		// (a)–(c): generic sniffs never overwrite a concrete stored type (FR-002/003)
		{"zip sniff keeps pptx", pptxMime, "application/zip", content, pptxMime, ReplaceOutcomeFallback, nil},
		{"octet-stream sniff keeps docx", docxMime, "application/octet-stream", content, docxMime, ReplaceOutcomeFallback, nil},
		{"text/plain sniff keeps xlsx", xlsxMime, "text/plain", content, xlsxMime, ReplaceOutcomeFallback, nil},
		{"text/plain sniff keeps markdown", "text/markdown", "text/plain", content, "text/markdown", ReplaceOutcomeFallback, nil},
		// equal concrete types accepted
		{"equal pptx accepted", pptxMime, pptxMime, content, pptxMime, ReplaceOutcomeAccepted, nil},
		{"png over png accepted (non-office control)", "image/png", "image/png", content, "image/png", ReplaceOutcomeAccepted, nil},
		// legacy rows: no trustworthy stored type to defend
		{"empty known accepts sniff", "", "image/png", content, "image/png", ReplaceOutcomeAccepted, nil},
		{"generic known self-heals to concrete sniff", "application/zip", docxMime, content, docxMime, ReplaceOutcomeAccepted, nil},
		{"generic known stays generic on generic sniff", "application/zip", "application/zip", content, "application/zip", ReplaceOutcomeAccepted, nil},
		// rejections
		{"empty content rejected", pptxMime, "", nil, "", ReplaceOutcomeRejectedEmpty, ErrEmptyContent},
		{"docx into pptx rejected", pptxMime, docxMime, content, "", ReplaceOutcomeRejectedMismatch, ErrMimeMismatch},
		{"jpeg into png rejected", "image/png", "image/jpeg", content, "", ReplaceOutcomeRejectedMismatch, ErrMimeMismatch},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &FileService{Logger: nopLogger, Processor: &mockProcessor{detectMIME: c.sniff}}
			gotMime, _, gotOutcome, err := svc.reconcileReplaceMIME(c.known, c.content)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotMime != c.wantMime {
				t.Errorf("mime = %q, want %q", gotMime, c.wantMime)
			}
			if gotOutcome != c.wantOutcome {
				t.Errorf("outcome = %q, want %q", gotOutcome, c.wantOutcome)
			}
		})
	}
}

// US1: a Collabora-style save (generic sniff) persists the document's known
// type, end to end through StoreAndLink.
func TestStoreAndLink_GenericSniff_PersistsKnownType(t *testing.T) {
	repo := &mockRepo{doc: model.Document{ID: uuid.New(), MimeType: pptxMime, ExternalID: "old"}}
	svc := &FileService{Logger: nopLogger,
		Repo:      repo,
		Storage:   &mockStorage{},
		Processor: &mockProcessor{detectMIME: "application/zip"},
	}

	result, err := svc.StoreAndLink(context.Background(), uuid.New(), buildReorderedOOXMLZip(t))
	if err != nil {
		t.Fatal(err)
	}
	if repo.lastUpdateFileMime != pptxMime {
		t.Errorf("persisted mime = %q, want %q (stored type must survive a generic sniff)", repo.lastUpdateFileMime, pptxMime)
	}
	if result.MimeType != pptxMime {
		t.Errorf("result mime = %q, want %q", result.MimeType, pptxMime)
	}
	if result.ReplaceOutcome != ReplaceOutcomeFallback {
		t.Errorf("outcome = %q, want %q", result.ReplaceOutcome, ReplaceOutcomeFallback)
	}
}

// T006(e) — FR-005 invariant property: across every accept outcome with a
// concrete stored type, the persisted MIME equals the pre-existing one.
func TestStoreAndLink_InvariantTypeNeverChanges(t *testing.T) {
	for _, sniff := range []string{"application/zip", "application/octet-stream", "text/plain", pptxMime} {
		repo := &mockRepo{doc: model.Document{ID: uuid.New(), MimeType: pptxMime, ExternalID: "old"}}
		svc := &FileService{Logger: nopLogger,
			Repo:      repo,
			Storage:   &mockStorage{},
			Processor: &mockProcessor{detectMIME: sniff},
		}
		if _, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("body")); err != nil {
			t.Fatalf("sniff %q: unexpected error: %v", sniff, err)
		}
		if repo.lastUpdateFileMime != pptxMime {
			t.Errorf("sniff %q: persisted mime = %q, want %q (FR-005 invariant)", sniff, repo.lastUpdateFileMime, pptxMime)
		}
	}
}

// T006(d) — FR-007 mid-write atomicity: UpdateFile fails after a successful
// Save → the old row stays intact and the old blob is never cleaned up.
func TestStoreAndLink_MidWriteFailure_LeavesRowAndOldBlobIntact(t *testing.T) {
	storage := &mockStorage{}
	repo := &mockRepo{
		doc:       model.Document{ID: uuid.New(), MimeType: pptxMime, ExternalID: "old"},
		updateErr: errors.New("db down"),
	}
	svc := &FileService{Logger: nopLogger,
		Repo:      repo,
		Storage:   storage,
		Processor: &mockProcessor{detectMIME: "application/zip"},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("body"))
	if err == nil {
		t.Fatal("expected error")
	}
	if storage.deleted {
		t.Error("old blob deleted on mid-write failure; prior content must stay retrievable (FR-007)")
	}
}

// US2 / T012 — empty replacement: rejected before any side effect (FR-003a, FR-007).
func TestStoreAndLink_EmptyContent_RejectedNoSideEffects(t *testing.T) {
	for _, content := range [][]byte{nil, {}} {
		storage := &mockStorage{}
		repo := &mockRepo{doc: model.Document{ID: uuid.New(), MimeType: pptxMime, ExternalID: "old"}}
		svc := &FileService{Logger: nopLogger,
			Repo:      repo,
			Storage:   storage,
			Processor: &mockProcessor{},
		}

		_, err := svc.StoreAndLink(context.Background(), uuid.New(), content)
		if !errors.Is(err, ErrEmptyContent) {
			t.Fatalf("err = %v, want ErrEmptyContent", err)
		}
		if storage.saved != nil {
			t.Error("blob written for rejected empty content")
		}
		if repo.updateFileCalls != 0 {
			t.Error("row mutated for rejected empty content")
		}
		if storage.deleted {
			t.Error("old blob deleted for rejected empty content")
		}
	}
}

// US3 / T015 — concrete type mismatch: rejected with the MIME pair, zero side effects (FR-004).
func TestStoreAndLink_MimeMismatch_RejectedNoSideEffects(t *testing.T) {
	storage := &mockStorage{}
	repo := &mockRepo{doc: model.Document{ID: uuid.New(), MimeType: pptxMime, ExternalID: "old"}}
	svc := &FileService{Logger: nopLogger,
		Repo:      repo,
		Storage:   storage,
		Processor: &mockProcessor{detectMIME: docxMime},
	}

	_, err := svc.StoreAndLink(context.Background(), uuid.New(), []byte("a real docx body"))
	if !errors.Is(err, ErrMimeMismatch) {
		t.Fatalf("err = %v, want ErrMimeMismatch", err)
	}
	var mismatch *MimeMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatal("error does not carry MimeMismatchError")
	}
	if mismatch.Known != pptxMime || mismatch.Detected != docxMime {
		t.Errorf("mismatch = {%q, %q}, want {%q, %q}", mismatch.Known, mismatch.Detected, pptxMime, docxMime)
	}
	if storage.saved != nil || repo.updateFileCalls != 0 || storage.deleted {
		t.Error("side effects observed for rejected mismatched content")
	}
}
