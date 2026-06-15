package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// Create-path MIME reconciliation (PR #13/#29): an OOXML package whose
// [Content_Types].xml lands past the 3072-byte sniff window degrades to
// application/zip, which is not in the bucket allow-list (the bucket lists the
// concrete office type) → a spurious 415. CompleteUpload now mirrors spec
// 019's reconcileReplaceMIME on the create allow-list: a generic sniff falls
// back to the caller's declared office type when that type is allowed.
//
// The fixture builder (buildReorderedOOXMLZip) and the pptx/docx/xlsx MIME
// constants live in replace_mime_test.go and are reused here verbatim — the
// two paths defend the same degradation, so they share the same fixture.

// Premise: the reordered OOXML fixture must sniff as application/zip under the
// REAL detector. The service tests below force the sniff via mockProcessor; if
// the fixture ever stopped degrading, those mocks would be testing nothing —
// this guard ties the mocked sniff value back to reality on the create side.
func TestReorderedOOXMLZip_SniffsZip_CreatePremise(t *testing.T) {
	detected := mimetype.Detect(buildReorderedOOXMLZip(t)).String()
	if normalizeMIME(detected) != "application/zip" {
		t.Fatalf("reordered OOXML zip sniffed as %q; the create fallback assumes application/zip", detected)
	}
}

// US: a pptx/docx/xlsx that sniffs as application/zip + a declared office MIME
// that the bucket allows → ACCEPTED, and the persisted document MimeType is
// the office type, never application/zip.
func TestCompleteUpload_GenericSniff_FallsBackToDeclaredOffice(t *testing.T) {
	cases := []struct {
		name     string
		declared string
	}{
		{"pptx", pptxMime},
		{"docx", docxMime},
		{"xlsx", xlsxMime},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storage := &mockStorage{}
			repo := &mockRepo{}
			// mockProcessor forces the sniff to application/zip, reproducing the
			// past-the-window degradation the premise test proves for real.
			svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage,
				Processor: &mockProcessor{detectMIME: "application/zip"}}
			input := model.CreateDocumentInput{
				DisplayName:     "deck.pptx",
				StorageBucketID: uuid.New(),
				AuthorizationID: uuid.New(),
			}

			su, err := svc.StageUpload(context.Background(), bytes.NewReader(buildReorderedOOXMLZip(t)), c.declared)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := svc.CompleteUpload(context.Background(), su, input, []string{c.declared}, 0)
			if err != nil {
				t.Fatalf("CompleteUpload rejected a valid office doc: %v", err)
			}
			if doc.MimeType != c.declared {
				t.Errorf("persisted mime = %q, want %q (must store the office type, not application/zip)", doc.MimeType, c.declared)
			}
			if !storage.stages[0].committed {
				t.Error("stage not committed for an accepted upload")
			}
		})
	}
}

// Guard: the fallback only fires for a GENUINELY allowed declared type. A
// zip-sniffing body whose declared type is NOT in the allow-list still 415s —
// the fix tolerates a missed office signature, it does not turn the allow-list
// into a suggestion.
func TestCompleteUpload_GenericSniff_DeclaredNotAllowed_StillRejects(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage,
		Processor: &mockProcessor{detectMIME: "application/zip"}}
	input := model.CreateDocumentInput{DisplayName: "x.zip", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}

	// Declared docx, but the bucket only allows pptx → reject.
	su, err := svc.StageUpload(context.Background(), bytes.NewReader(buildReorderedOOXMLZip(t)), docxMime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(context.Background(), su, input, []string{pptxMime}, 0); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
	assertSingleAbortedStage(t, storage)
}

// Guard: a zip-sniffing body with NO declared type (empty Content-Type) and a
// bucket that does not allow application/zip still 415s — there is nothing
// trustworthy to fall back to.
func TestCompleteUpload_GenericSniff_NoDeclared_StillRejects(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage,
		Processor: &mockProcessor{detectMIME: "application/zip"}}
	input := model.CreateDocumentInput{DisplayName: "x.bin", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}

	su, err := svc.StageUpload(context.Background(), bytes.NewReader(buildReorderedOOXMLZip(t)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteUpload(context.Background(), su, input, []string{pptxMime}, 0); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType", err)
	}
	assertSingleAbortedStage(t, storage)
}

// Guard: the transcode path is untouched. When an image is transcoded,
// su.MimeType (the encoder OUTPUT) differs from su.DetectedMIME (the original
// input); reconciliation must not run and must not clobber the output MIME.
// We force a generic DETECTED type and a concrete declared+output type to
// prove the guard keys on su.MimeType != su.DetectedMIME, not on the sniff
// alone.
func TestCompleteUpload_TranscodeOutput_NotReconciled(t *testing.T) {
	storage := &mockStorage{}
	svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage,
		Processor: &mockProcessor{detectMIME: "application/zip"}}
	input := model.CreateDocumentInput{DisplayName: "img.jpg", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}

	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("body")), "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a transcode: the persisted output is image/jpeg while the input
	// degraded to application/zip. The bucket allows the original detected type.
	su.MimeType = "image/jpeg"
	declared := "image/png"
	su.DeclaredMIME = declared
	if _, err := svc.CompleteUpload(context.Background(), su, input, []string{"application/zip"}, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if su.MimeType != "image/jpeg" {
		t.Errorf("transcode output MIME clobbered: got %q, want image/jpeg", su.MimeType)
	}
}
