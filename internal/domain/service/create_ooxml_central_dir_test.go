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

// Create path: recover the canonical OOXML MIME from a reordered office zip's
// central directory when the 3072-byte front-window sniff has degraded to
// application/zip ([Content_Types].xml past the window). Regression for the
// spurious 415 on create. The closed PR #42 trusted the declared MIME; here
// we instead verify the staged content's central directory.

// buildOOXMLZipWithMarker builds a synthetic OOXML-shaped package whose office
// marker entries sit *after* an 8 KiB leading entry, so the front-window sniff
// can only see application/zip (proven by the premise test below). markerDir
// is the family directory ("ppt/", "word/", "xl/"). The leading entry mirrors
// the real Collabora trigger: a stored docProps thumbnail (a pptx leading with
// docProps/_rels/customXml is what surfaced this bug in production).
func buildOOXMLZipWithMarker(t *testing.T, markerDir string) []byte {
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
		e, err := zw.Create(fmt.Sprintf("%spart%d.xml", markerDir, i+1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte(`<?xml version="1.0"?><x/>`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildPlainZip builds a zip with real entries but NO [Content_Types].xml and
// no office marker directory — a genuine non-office archive. Used to prove the
// recovery path does not blindly accept a mislabeled zip (the key improvement
// over the closed PR #42).
func buildPlainZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	hdr := &zip.FileHeader{Name: "data/blob.bin", Method: zip.Store}
	f, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xAA, 0x55}, 4096)); err != nil {
		t.Fatal(err)
	}
	r, err := zw.Create("README.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("just a plain zip")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Premise: every fixture must genuinely sniff as application/zip under the real
// detector, so the create tests prove the recovery path rather than a trivially
// correct front-window sniff.
func TestCreateOOXMLFixtures_SniffGeneric(t *testing.T) {
	fixtures := map[string][]byte{
		"pptx":     buildOOXMLZipWithMarker(t, "ppt/"),
		"docx":     buildOOXMLZipWithMarker(t, "word/"),
		"xlsx":     buildOOXMLZipWithMarker(t, "xl/"),
		"plainzip": buildPlainZip(t),
	}
	for name, content := range fixtures {
		detected := mimetype.Detect(content).String()
		if model.NormalizeMIME(detected) != "application/zip" {
			t.Errorf("%s fixture sniffed as %q, want application/zip (premise of the recovery path)", name, detected)
		}
	}
}

// detectZipOfficeMIME maps each marker family to the canonical office MIME, and
// rejects a plain zip.
func TestDetectZipOfficeMIME(t *testing.T) {
	cases := []struct {
		name     string
		content  []byte
		wantMIME string
		wantOK   bool
	}{
		{"pptx", buildOOXMLZipWithMarker(t, "ppt/"), model.OfficeExtToMIME[".pptx"], true},
		{"docx", buildOOXMLZipWithMarker(t, "word/"), model.OfficeExtToMIME[".docx"], true},
		{"xlsx", buildOOXMLZipWithMarker(t, "xl/"), model.OfficeExtToMIME[".xlsx"], true},
		{"plain zip rejected", buildPlainZip(t), "", false},
		{"garbage rejected", []byte("not a zip at all"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, ok := detectZipOfficeMIME(bytes.NewReader(tc.content), int64(len(tc.content)))
			if ok != tc.wantOK || mime != tc.wantMIME {
				t.Fatalf("detectZipOfficeMIME = (%q, %v), want (%q, %v)", mime, ok, tc.wantMIME, tc.wantOK)
			}
		})
	}
}

// completeStagedZip stages content whose front-window sniff is forced to
// application/zip (the degraded-sniff condition), then completes the upload
// against allowed. Returns the resulting document and error.
func completeStagedZip(t *testing.T, content []byte, allowed []string) (*model.Document, *mockStorage, error) {
	t.Helper()
	storage := &mockStorage{}
	repo := &mockRepo{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{detectMIME: "application/zip"}}

	su, err := svc.StageUpload(context.Background(), bytes.NewReader(content), "", false)
	if err != nil {
		t.Fatalf("StageUpload: %v", err)
	}
	if su.DetectedMIME != "application/zip" {
		t.Fatalf("precondition: DetectedMIME = %q, want application/zip", su.DetectedMIME)
	}
	input := model.CreateDocumentInput{DisplayName: "deck.pptx", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	doc, err := svc.CompleteUpload(context.Background(), su, input, allowed, 0)
	return doc, storage, err
}

// Recovered OOXML packages whose office type is allowed are ACCEPTED and the
// persisted document MIME is the office type, not application/zip.
func TestCompleteUpload_RecoversOOXMLFromCentralDir(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		wantMIME string
	}{
		{"pptx", "ppt/", model.OfficeExtToMIME[".pptx"]},
		{"docx", "word/", model.OfficeExtToMIME[".docx"]},
		{"xlsx", "xl/", model.OfficeExtToMIME[".xlsx"]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := buildOOXMLZipWithMarker(t, tc.marker)
			doc, _, err := completeStagedZip(t, content, []string{tc.wantMIME})
			if err != nil {
				t.Fatalf("CompleteUpload: %v, want accepted", err)
			}
			if doc.MimeType != tc.wantMIME {
				t.Errorf("doc.MimeType = %q, want %q (content-derived office type, not application/zip)", doc.MimeType, tc.wantMIME)
			}
		})
	}
}

// A plain non-office zip declared as pptx with an allow-list of [pptx] is still
// REJECTED: recovery does not invent an office type, so the existing allow-list
// check rejects the unrecovered application/zip. This is the key improvement
// over the closed PR #42 (which trusted the declared MIME).
func TestCompleteUpload_PlainZipDeclaredOfficeRejected(t *testing.T) {
	content := buildPlainZip(t)
	doc, storage, err := completeStagedZip(t, content, []string{model.OfficeExtToMIME[".pptx"]})
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType (mislabeled non-office zip must be rejected)", err)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil on rejection", doc)
	}
	assertSingleAbortedStage(t, storage)
}

// An office zip whose recovered type is NOT in the bucket allow-list is
// REJECTED — recovery feeds the allow-list, it does not bypass it.
func TestCompleteUpload_RecoveredOfficeNotAllowedRejected(t *testing.T) {
	content := buildOOXMLZipWithMarker(t, "ppt/") // recovers as pptx
	doc, storage, err := completeStagedZip(t, content, []string{model.OfficeExtToMIME[".docx"]})
	if !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("err = %v, want ErrUnsupportedMediaType (recovered pptx not in [docx] allow-list)", err)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil on rejection", doc)
	}
	assertSingleAbortedStage(t, storage)
}

// When StagedReaderAt fails (an infrastructure read error — FS sync, or a
// future object-store ranged read), OOXML recovery is skipped rather than
// turned into a hard error: the upload falls through to plain application/zip
// allow-list validation. An office package whose staged bytes are unreadable is
// therefore treated as application/zip — accepted when zip is allowed, rejected
// only by the ordinary allow-list, never by a synthesised inspection error.
func TestCompleteUpload_StagedReaderErrorSkipsRecovery(t *testing.T) {
	readErr := errors.New("staged reader unavailable")
	content := buildOOXMLZipWithMarker(t, "ppt/") // would recover as pptx if readable

	// application/zip allowed: recovery is skipped, content validates as zip.
	t.Run("zip allowed → accepted as application/zip", func(t *testing.T) {
		storage := &mockStorage{stageReaderErr: readErr}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: &mockProcessor{detectMIME: "application/zip"}}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader(content), "", false)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		input := model.CreateDocumentInput{DisplayName: "deck.pptx", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
		doc, err := svc.CompleteUpload(context.Background(), su, input, []string{"application/zip"}, 0)
		if err != nil {
			t.Fatalf("CompleteUpload: %v, want accepted (recovery skipped, not a hard error)", err)
		}
		if doc.MimeType != "application/zip" {
			t.Errorf("doc.MimeType = %q, want application/zip (recovery skipped on reader error)", doc.MimeType)
		}
	})

	// office-only allow-list: recovery skipped, so the unrecovered application/zip
	// is rejected by the ordinary allow-list — not by an inspection error.
	t.Run("office-only allow-list → rejected by allow-list", func(t *testing.T) {
		storage := &mockStorage{stageReaderErr: readErr}
		svc := &FileService{Logger: nopLogger, Repo: &mockRepo{}, Storage: storage, Processor: &mockProcessor{detectMIME: "application/zip"}}
		su, err := svc.StageUpload(context.Background(), bytes.NewReader(content), "", false)
		if err != nil {
			t.Fatalf("StageUpload: %v", err)
		}
		input := model.CreateDocumentInput{DisplayName: "deck.pptx", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
		doc, err := svc.CompleteUpload(context.Background(), su, input, []string{model.OfficeExtToMIME[".pptx"]}, 0)
		if !errors.Is(err, ErrUnsupportedMediaType) {
			t.Fatalf("err = %v, want ErrUnsupportedMediaType (allow-list rejects unrecovered zip)", err)
		}
		if doc != nil {
			t.Errorf("doc = %+v, want nil on rejection", doc)
		}
		assertSingleAbortedStage(t, storage)
	})
}

// The image-transcode path's persisted MIME is unaffected: when MimeType is the
// transcode output (≠ DetectedMIME), the zip-recovery gate never fires. We
// simulate it by leaving DetectedMIME generic but MimeType set to the transcode
// result, mirroring stageContent's image branch.
func TestCompleteUpload_TranscodePathMIMEUnaffected(t *testing.T) {
	storage := &mockStorage{}
	repo := &mockRepo{}
	svc := &FileService{Logger: nopLogger, Repo: repo, Storage: storage, Processor: &mockProcessor{}}

	// Stage some bytes via a normal pass-through to get a usable stage.
	su, err := svc.StageUpload(context.Background(), bytes.NewReader([]byte("transcoded jpeg bytes")), "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate the image-transcode outcome: DetectedMIME stays the (irrelevant)
	// sniff, MimeType is the encoder output and differs from DetectedMIME.
	su.DetectedMIME = "application/zip"
	su.MimeType = "image/jpeg"

	input := model.CreateDocumentInput{DisplayName: "photo.jpg", StorageBucketID: uuid.New(), AuthorizationID: uuid.New()}
	doc, err := svc.CompleteUpload(context.Background(), su, input, []string{"application/zip"}, 0)
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if doc.MimeType != "image/jpeg" {
		t.Errorf("doc.MimeType = %q, want image/jpeg (transcode output must not be rewritten by zip recovery)", doc.MimeType)
	}
}
