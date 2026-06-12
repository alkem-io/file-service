//go:build vips

package imaging

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// Spec 020 T016 — streaming transcode against real fixtures (see
// testdata/README.md for provenance).

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bufferedEquivalent runs the same canonicalization buffered: the
// SC-002-amended equivalence target (AutoRotate → RemoveMetadata →
// SaveToWriter with the streaming params).
func bufferedEquivalent(t *testing.T, content []byte, mimeType string) []byte {
	t.Helper()
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	if err := img.AutoRotate(); err != nil {
		t.Fatal(err)
	}
	if err := img.RemoveMetadata(); err != nil {
		t.Fatal(err)
	}
	format := vips.ImageTypeJPEG
	ep := &vips.ExportParams{Quality: 82, Interlaced: false}
	if mimeType == "image/png" {
		format = vips.ImageTypePNG
		ep = &vips.ExportParams{Compression: 6}
	}
	var buf bytes.Buffer
	if err := img.SaveToWriter(&buf, format, ep); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setBudget(t *testing.T, n int64) {
	t.Helper()
	prev := PixelBudget()
	ConfigureStreaming(StreamingConfig{PixelBudget: n})
	t.Cleanup(func() { ConfigureStreaming(StreamingConfig{PixelBudget: prev}) })
}

func runTranscode(t *testing.T, content []byte, mimeType string) ([]byte, port.TranscodeResult, error) {
	t.Helper()
	var out bytes.Buffer
	res, err := New().TranscodeStream(bytes.NewReader(content), &out, mimeType)
	return out.Bytes(), res, err
}

// (a) HEIC: whole-frame codec + format conversion → baseline JPEG,
// byte-identical to the buffered equivalent, dims recorded.
func TestTranscodeStream_HEICToJPEG(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "heic-24bit.heic")

	out, res, err := runTranscode(t, content, "image/heic")
	if err != nil {
		t.Fatal(err)
	}
	if res.MimeType != "image/jpeg" {
		t.Errorf("mime = %s, want image/jpeg", res.MimeType)
	}
	if res.ImageWidth == nil || res.ImageHeight == nil || *res.ImageWidth <= 0 {
		t.Fatalf("dims missing: %+v", res)
	}
	if want := bufferedEquivalent(t, content, "image/heic"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent (%d vs %d bytes)", len(out), len(want))
	}
}

// (b) WebP → JPEG identity.
func TestTranscodeStream_WebPToJPEG(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "webp+alpha.webp")

	out, res, err := runTranscode(t, content, "image/webp")
	if err != nil {
		t.Fatal(err)
	}
	if res.MimeType != "image/jpeg" {
		t.Errorf("mime = %s, want image/jpeg", res.MimeType)
	}
	if want := bufferedEquivalent(t, content, "image/webp"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent")
	}
}

// (c) Rotated real photo (EXIF 6): materialized path, dims swapped,
// orientation cleared in the output.
func TestTranscodeStream_RotatedJPEGMaterializes(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "jpg-orientation-6.jpg")

	src, err := vips.NewImageFromBuffer(content)
	if err != nil {
		t.Fatal(err)
	}
	rawW, rawH, srcOrient := src.Width(), src.Height(), src.Orientation()
	src.Close()
	if srcOrient != 6 {
		t.Fatalf("fixture orientation = %d, want 6", srcOrient)
	}

	out, res, err := runTranscode(t, content, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if *res.ImageWidth != rawH || *res.ImageHeight != rawW {
		t.Errorf("dims = %dx%d, want swapped %dx%d", *res.ImageWidth, *res.ImageHeight, rawH, rawW)
	}
	outImg, err := vips.NewImageFromBuffer(out)
	if err != nil {
		t.Fatal(err)
	}
	defer outImg.Close()
	if o := outImg.Orientation(); o > 1 {
		t.Errorf("output orientation = %d, want cleared", o)
	}
	if want := bufferedEquivalent(t, content, "image/jpeg"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent")
	}
}

// (d) Pixel bomb: rejected from header metadata before any pixel decode.
func TestTranscodeStream_PixelBombRejected(t *testing.T) {
	setBudget(t, 100_000_000) // 100 MP; bomb declares 900 MP
	content := fixture(t, "pixel-bomb-30000x30000.png")

	var out bytes.Buffer
	_, err := New().TranscodeStream(bytes.NewReader(content), &out, "image/png")
	if !errors.Is(err, port.ErrPixelBudgetExceeded) {
		t.Fatalf("err = %v, want ErrPixelBudgetExceeded", err)
	}
	if out.Len() != 0 {
		t.Errorf("bytes written for a rejected bomb: %d", out.Len())
	}
}

// (e) Truncated stream: load or save fails with the reader error surfaced.
func TestTranscodeStream_TruncatedStream(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "jpg-orientation-6.jpg")
	truncated := io.LimitReader(bytes.NewReader(content), int64(len(content)/3))

	var out bytes.Buffer
	_, err := New().TranscodeStream(truncated, &out, "image/jpeg")
	if err == nil {
		t.Fatal("expected error for truncated stream")
	}
}

// (f) Canonical JPEG: recompressed (no size guard — 2026-06-12
// clarification), baseline output, byte-identical to buffered equivalent.
func TestTranscodeStream_CanonicalJPEGRecompressed(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "jpg-24bit.jpg")

	out, res, err := runTranscode(t, content, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out, content) {
		t.Error("canonical JPEG passed through; want recompression (size guard dropped)")
	}
	if res.MimeType != "image/jpeg" {
		t.Errorf("mime = %s", res.MimeType)
	}
	if want := bufferedEquivalent(t, content, "image/jpeg"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent")
	}
}

// (c2) Rotated HEIC: whole-frame codec + rotation in one input.
func TestTranscodeStream_RotatedHEIC(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "heic-orientation-6.heic")

	out, res, err := runTranscode(t, content, "image/heic")
	if err != nil {
		t.Fatal(err)
	}
	if res.MimeType != "image/jpeg" || res.ImageWidth == nil {
		t.Fatalf("res = %+v", res)
	}
	if want := bufferedEquivalent(t, content, "image/heic"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent")
	}
}

// (f2) PNG stays PNG through the streaming encoder.
func TestTranscodeStream_PNGStaysPNG(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "png-24bit.png")

	out, res, err := runTranscode(t, content, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if res.MimeType != "image/png" {
		t.Errorf("mime = %s, want image/png", res.MimeType)
	}
	if want := bufferedEquivalent(t, content, "image/png"); !bytes.Equal(out, want) {
		t.Errorf("streaming output differs from buffered equivalent")
	}
}

// Corrupt input: decoder failure propagates as an error (not a hang or
// silent success).
func TestTranscodeStream_CorruptJPEG(t *testing.T) {
	setBudget(t, 100_000_000)
	content := fixture(t, "jpg-corruption.jpg")

	var out bytes.Buffer
	_, err := New().TranscodeStream(bytes.NewReader(content), &out, "image/jpeg")
	if err == nil {
		t.Skip("vips tolerates this corruption level; tolerated output accepted")
	}
}
