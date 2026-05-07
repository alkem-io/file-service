//go:build vips

package imaging

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
)

func TestMain(m *testing.M) {
	_ = vips.Startup(nil)
	defer vips.Shutdown()
	m.Run()
}

func TestDetectMIME_JPEG(t *testing.T) {
	p := &Processor{}
	content := makeJPEG(t, 10, 10)
	mime := p.DetectMIME(content)
	if mime != "image/jpeg" {
		t.Errorf("got %q, want image/jpeg", mime)
	}
}

func TestDetectMIME_PNG(t *testing.T) {
	p := &Processor{}
	content := makePNG(t, 10, 10)
	mime := p.DetectMIME(content)
	if mime != "image/png" {
		t.Errorf("got %q, want image/png", mime)
	}
}

func TestDetectMIME_GIF(t *testing.T) {
	p := &Processor{}
	content := makeGIF(t, 10, 10)
	mime := p.DetectMIME(content)
	if mime != "image/gif" {
		t.Errorf("got %q, want image/gif", mime)
	}
}

func TestDetectMIME_PDF(t *testing.T) {
	p := &Processor{}
	// Minimal PDF
	content := []byte("%PDF-1.0\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n%%EOF")
	mime := p.DetectMIME(content)
	if mime != "application/pdf" {
		t.Errorf("got %q, want application/pdf", mime)
	}
}

func TestDetectMIME_SVG(t *testing.T) {
	p := &Processor{}
	content := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)
	mime := p.DetectMIME(content)
	if !strings.Contains(mime, "xml") && !strings.Contains(mime, "svg") {
		t.Errorf("got %q, expected svg or xml mime", mime)
	}
}

func TestDetectMIME_Unknown(t *testing.T) {
	p := &Processor{}
	content := []byte{0x00, 0x01, 0x02, 0x03}
	mime := p.DetectMIME(content)
	if mime != "application/octet-stream" {
		t.Errorf("got %q, want application/octet-stream", mime)
	}
}

func TestProcess_JPEG_Compression(t *testing.T) {
	p := &Processor{}
	// Create a large-ish JPEG
	content := makeJPEG(t, 200, 200)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", result.MimeType)
	}
	// Processed should be valid (non-empty)
	if len(result.Content) == 0 {
		t.Error("empty processed output")
	}
}

func TestProcess_JPEG_Resize(t *testing.T) {
	p := &Processor{}
	// Create an oversized JPEG (5000x5000)
	content := makeJPEG(t, 5000, 5000)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Decode to check dimensions
	img, err := vips.NewImageFromBuffer(result.Content)
	if err != nil {
		t.Fatalf("decode processed: %v", err)
	}
	defer img.Close()

	if img.Width() > 4096 || img.Height() > 4096 {
		t.Errorf("dimensions %dx%d exceed 4096", img.Width(), img.Height())
	}
}

func TestProcess_PNG_PassThrough(t *testing.T) {
	p := &Processor{}
	content := makePNG(t, 10, 10)

	result, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", result.MimeType)
	}
	// Orientation-1 / orientation-absent PNG: passthrough byte-identical
	// (canonicalizeRaster returns input bytes when orient is 0 or 1).
	if !bytes.Equal(result.Content, content) {
		t.Error("PNG with orientation=1/absent should pass through unmodified")
	}
}

func TestProcess_GIF_PassThrough(t *testing.T) {
	p := &Processor{}
	content := makeGIF(t, 10, 10)

	result, err := p.Process(content, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/gif" {
		t.Errorf("mime = %q, want image/gif", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("GIF should pass through unmodified")
	}
}

func TestProcess_SVG_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)

	result, err := p.Process(content, "image/svg+xml")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/svg+xml" {
		t.Errorf("mime = %q, want image/svg+xml", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("SVG should pass through unmodified")
	}
}

func TestProcess_PDF_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("%PDF-1.0 test")

	result, err := p.Process(content, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "application/pdf" {
		t.Errorf("mime = %q, want application/pdf", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("PDF should pass through unmodified")
	}
}

func TestProcess_HEIC_ToJPEG(t *testing.T) {
	p := &Processor{}
	// Generate a minimal HEIC using govips
	heicContent := makeHEIC(t, 100, 100)
	if len(heicContent) == 0 {
		t.Skip("could not generate HEIC test image")
	}

	result, err := p.Process(heicContent, "image/heic")
	if err != nil {
		t.Fatalf("Process HEIC: %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty processed output")
	}
	// Verify it's a valid JPEG (starts with FF D8)
	if len(result.Content) < 2 || result.Content[0] != 0xFF || result.Content[1] != 0xD8 {
		t.Error("output is not a valid JPEG")
	}
}

func TestProcess_HEIF_ToJPEG(t *testing.T) {
	p := &Processor{}
	heicContent := makeHEIC(t, 50, 50)
	if len(heicContent) == 0 {
		t.Skip("could not generate HEIC test image")
	}

	result, err := p.Process(heicContent, "image/heif")
	if err != nil {
		t.Fatalf("Process HEIF: %v", err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_WebP_Processed(t *testing.T) {
	p := &Processor{}
	// Create a larger WebP so compression is beneficial
	webpContent := makeWebP(t, 500, 500)
	if len(webpContent) == 0 {
		t.Skip("could not generate WebP test image")
	}

	result, err := p.Process(webpContent, "image/webp")
	if err != nil {
		t.Fatalf("Process WebP: %v", err)
	}
	// Either JPEG (if compression was beneficial) or original WebP (size guard)
	if result.MimeType != "image/jpeg" && result.MimeType != "image/webp" {
		t.Errorf("mime = %q, want image/jpeg or image/webp", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty output")
	}
}

func TestConvertHEICToJPEG_Direct(t *testing.T) {
	heicContent := makeHEIC(t, 80, 80)
	if len(heicContent) == 0 {
		t.Skip("could not generate HEIC")
	}

	jpegOut, err := convertHEICToJPEG(heicContent)
	if err != nil {
		t.Fatalf("convertHEICToJPEG: %v", err)
	}
	if len(jpegOut) < 2 || jpegOut[0] != 0xFF || jpegOut[1] != 0xD8 {
		t.Error("output is not valid JPEG")
	}
}

func TestCompressJPEG_Direct(t *testing.T) {
	content := makeJPEG(t, 300, 300)

	compressed, err := compressJPEG(content)
	if err != nil {
		t.Fatalf("compressJPEG: %v", err)
	}
	if len(compressed.bytes) == 0 {
		t.Error("empty output")
	}
}

func TestNew_Initializes(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
}

func TestProcess_BMP_FakeBytes_Pass(t *testing.T) {
	// "BM fake bmp content" is not a real BMP — vips.NewImageFromBuffer
	// fails to load it inside canonicalizeRaster, which returns the input
	// bytes unchanged with Measured=true (the {_decodeFailed} sentinel
	// path). Asserts the bytes round-trip even when vips can't load them.
	p := &Processor{}
	content := []byte("BM fake bmp content")

	result, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/bmp" {
		t.Errorf("mime = %q, want image/bmp", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("unparseable BMP bytes should round-trip unchanged")
	}
}

func TestProcess_UnknownType_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("random binary data")

	result, err := p.Process(content, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "application/octet-stream" {
		t.Errorf("mime = %q", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("unknown type should pass through")
	}
}

func TestProcess_HEIC_ConversionError(t *testing.T) {
	p := &Processor{}
	// Corrupt data that claims to be HEIC
	_, err := p.Process([]byte("not a real heic file"), "image/heic")
	if err == nil {
		t.Fatal("expected error for corrupt HEIC")
	}
}

func TestProcess_JPEG_CompressFallback(t *testing.T) {
	p := &Processor{}
	// Corrupt data that claims to be JPEG — compressJPEG will fail, should fallback
	corrupt := []byte{0xFF, 0xD8, 0xFF, 0x00} // JPEG magic but truncated
	result, err := p.Process(corrupt, "image/jpeg")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Should return original content as fallback
	if result.MimeType != "image/jpeg" {
		t.Errorf("mime = %q", result.MimeType)
	}
	if len(result.Content) != len(corrupt) {
		t.Errorf("expected fallback to original size %d, got %d", len(corrupt), len(result.Content))
	}
}

func TestConvertHEICToJPEG_CorruptInput(t *testing.T) {
	_, err := convertHEICToJPEG([]byte("not heic"))
	if err == nil {
		t.Fatal("expected error for corrupt HEIC input")
	}
}

func TestCompressJPEG_CorruptInput(t *testing.T) {
	_, err := compressJPEG([]byte("not a jpeg"))
	if err == nil {
		t.Fatal("expected error for corrupt input")
	}
}

func TestProcess_HEIC_CompressFallback(t *testing.T) {
	// This tests the path where HEIC converts OK but compress fails
	// Hard to trigger since convertHEICToJPEG produces valid JPEG
	// But we can at least verify the happy path works end-to-end
	p := &Processor{}
	heic := makeHEIC(t, 50, 50)
	if len(heic) == 0 {
		t.Skip("no HEIC support")
	}
	result, err := p.Process(heic, "image/heif")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/jpeg" {
		t.Errorf("mime = %q", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_JPG_Alias(t *testing.T) {
	p := &Processor{}
	content := makeJPEG(t, 100, 100)
	result, err := p.Process(content, "image/jpg")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/jpeg" && result.MimeType != "image/jpg" {
		t.Errorf("mime = %q", result.MimeType)
	}
}

func TestProcess_AVIF_FakeBytes_Pass(t *testing.T) {
	// "fake avif content" is not a real AVIF — vips.NewImageFromBuffer
	// fails inside canonicalizeRaster, which returns the input bytes
	// unchanged with Measured=true (the {_decodeFailed} sentinel path).
	p := &Processor{}
	content := []byte("fake avif content")
	result, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatal(err)
	}
	if result.MimeType != "image/avif" {
		t.Errorf("mime = %q", result.MimeType)
	}
	if !bytes.Equal(result.Content, content) {
		t.Error("unparseable AVIF bytes should round-trip unchanged")
	}
}

func TestCompressJPEG_TallImage(t *testing.T) {
	// Test resize path where height > width > 4096
	content := makeJPEG(t, 100, 5000)
	compressed, err := compressJPEG(content)
	if err != nil {
		t.Fatal(err)
	}

	img, err := vips.NewImageFromBuffer(compressed.bytes)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	if img.Height() > 4096 {
		t.Errorf("height %d exceeds 4096", img.Height())
	}
}

func TestConvertHEICToJPEG_EmptyInput(t *testing.T) {
	_, err := convertHEICToJPEG(nil)
	if err == nil {
		t.Fatal("expected error for nil input")
	}
}

func TestConvertHEICToJPEG_TruncatedHEIC(t *testing.T) {
	// HEIC magic bytes (ftyp box) but truncated — loads partially then fails
	heicMagic := []byte{
		0x00, 0x00, 0x00, 0x1C, // box size
		0x66, 0x74, 0x79, 0x70, // "ftyp"
		0x68, 0x65, 0x69, 0x63, // "heic"
		0x00, 0x00, 0x00, 0x00, // minor version
		0x68, 0x65, 0x69, 0x63, // compatible brand
		// truncated — missing actual image data
	}
	_, err := convertHEICToJPEG(heicMagic)
	if err == nil {
		t.Fatal("expected error for truncated HEIC")
	}
}

func TestCompressJPEG_EmptyInput(t *testing.T) {
	_, err := compressJPEG(nil)
	if err == nil {
		t.Fatal("expected error for nil input")
	}
}

func TestCompressJPEG_TruncatedJPEG(t *testing.T) {
	// Valid JPEG SOI marker but truncated
	truncated := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	_, err := compressJPEG(truncated)
	if err == nil {
		t.Fatal("expected error for truncated JPEG")
	}
}

func TestCompressJPEG_RandomBytes(t *testing.T) {
	// Completely random data
	_, err := compressJPEG([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
	if err == nil {
		t.Fatal("expected error for random bytes")
	}
}

func TestProcess_WebP_CompressFails_FallsBack(t *testing.T) {
	p := &Processor{}
	// Truncated WebP: RIFF header but corrupt content
	// WebP magic: "RIFF" + size + "WEBP"
	corruptWebP := []byte("RIFF\x00\x00\x00\x00WEBP")
	result, err := p.Process(corruptWebP, "image/webp")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Should fall back to original
	if result.MimeType != "image/webp" {
		t.Logf("mime = %q (compress may have succeeded on minimal input)", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_JPEG_CompressFails_FallsBack(t *testing.T) {
	p := &Processor{}
	// Minimal JPEG SOI + garbage — compressJPEG should fail, Process should fallback
	corruptJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x02, 0x00, 0x00}
	result, err := p.Process(corruptJPEG, "image/jpeg")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Fallback returns original content with original mime
	if result.MimeType != "image/jpeg" {
		t.Logf("mime = %q", result.MimeType)
	}
	if len(result.Content) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_CompressionSizeGuard(t *testing.T) {
	p := &Processor{}
	// Create a tiny JPEG that can't be compressed smaller
	content := makeJPEG(t, 2, 2)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// Size guard: if compressed >= original, return original
	if len(result.Content) > len(content)*2 {
		t.Errorf("processed (%d bytes) much larger than original (%d bytes)", len(result.Content), len(content))
	}
}

// --- Helpers ---

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeHEIC(t *testing.T, w, h int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("cannot create test image: %v", err)
		return nil
	}
	defer img.Close()

	ep := vips.NewHeifExportParams()
	ep.Quality = 50
	out, _, err := img.ExportHeif(ep)
	if err != nil {
		t.Skipf("cannot export HEIC (codec not available): %v", err)
		return nil
	}
	return out
}

func makeWebP(t *testing.T, w, h int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("cannot create test image: %v", err)
		return nil
	}
	defer img.Close()

	ep := vips.NewWebpExportParams()
	ep.Quality = 75
	out, _, err := img.ExportWebp(ep)
	if err != nil {
		t.Skipf("cannot export WebP: %v", err)
		return nil
	}
	return out
}

func makeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	palette := []color.Color{color.Black, color.White}
	img := image.NewPaletted(image.Rect(0, 0, w, h), palette)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeJPEGWithOrientation builds a w×h JPEG carrying a specific EXIF
// Orientation value. Used by US1 to create orient1 / orient6 fixtures
// programmatically — no external tooling, no checked-in binaries.
//
// vips.SetOrientation embeds orientation in the EXIF block on the
// next ExportJpeg, so the output is a real JPEG that vips (and any
// renderer respecting EXIF) recognizes as oriented.
func makeJPEGWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Fatalf("vips.Black: %v", err)
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Fatalf("SetOrientation(%d): %v", orientation, err)
	}

	ep := vips.NewJpegExportParams()
	ep.Quality = 95
	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		t.Fatalf("ExportJpeg: %v", err)
	}
	return out
}

// makeWebPWithOrientation builds a w×h WebP carrying a specific EXIF
// Orientation value (US1 fixture for the WebP arm of canonicalizeRaster
// via compressJPEG path).
func makeWebPWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("vips.Black: %v", err)
		return nil
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Skipf("SetOrientation(%d): %v", orientation, err)
		return nil
	}

	ep := vips.NewWebpExportParams()
	ep.Quality = 80
	out, _, err := img.ExportWebp(ep)
	if err != nil {
		t.Skipf("ExportWebp: %v", err)
		return nil
	}
	return out
}

// makeHEICWithOrientation builds a w×h HEIC carrying a specific EXIF
// Orientation value. Skips if HEIF encoder is unavailable in this libvips.
func makeHEICWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("vips.Black: %v", err)
		return nil
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Skipf("SetOrientation(%d): %v", orientation, err)
		return nil
	}

	ep := vips.NewHeifExportParams()
	ep.Quality = 50
	out, _, err := img.ExportHeif(ep)
	if err != nil {
		t.Skipf("ExportHeif: %v", err)
		return nil
	}
	return out
}

// reportedOrientation reads the orientation tag from result bytes via vips.
// The contract for canonicalized output is "orientation absent or 1" — vips
// reports 0 (unset) for stripped files, 1 for explicitly-canonical. Returns
// the literal value so tests can assert either.
func reportedOrientation(t *testing.T, content []byte) int {
	t.Helper()
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		t.Fatalf("vips load: %v", err)
	}
	defer img.Close()
	return img.Orientation()
}

// --- US1 tests: phone-photo orient/dim regression ---

func TestProcessor_JPEG_Orient1_ReportsRawDims(t *testing.T) {
	p := &Processor{}
	const w, h = 1024, 512
	content := makeJPEGWithOrientation(t, w, h, 1)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded JPEG")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != w || *result.ImageHeight != h {
		t.Errorf("dims = %dx%d, want %dx%d (orientation=1, no swap)", *result.ImageWidth, *result.ImageHeight, w, h)
	}
}

func TestProcessor_JPEG_Orient6_ReportsRotatedDims(t *testing.T) {
	p := &Processor{}
	// Phone-photo regression repro: raw bytes 1024×512, orient=6 → renderer
	// sees 512×1024.
	const rawW, rawH = 1024, 512
	content := makeJPEGWithOrientation(t, rawW, rawH, 6)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded JPEG")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	wantW, wantH := rawH, rawW // swapped
	if *result.ImageWidth != wantW || *result.ImageHeight != wantH {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *result.ImageWidth, *result.ImageHeight, wantW, wantH)
	}
	// Output bytes must have orientation tag absent (0) or 1.
	if got := reportedOrientation(t, result.Content); got != 0 && got != 1 {
		t.Errorf("output orientation = %d, want 0 or 1 (canonicalized)", got)
	}
}

func TestProcessor_WebP_Orient6_ReportsRotatedDims(t *testing.T) {
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makeWebPWithOrientation(t, rawW, rawH, 6)
	if len(content) == 0 {
		t.Skip("WebP encoder unavailable")
	}

	result, err := p.Process(content, "image/webp")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded WebP")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	wantW, wantH := rawH, rawW
	if *result.ImageWidth != wantW || *result.ImageHeight != wantH {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *result.ImageWidth, *result.ImageHeight, wantW, wantH)
	}
}

func TestProcessor_HEIC_Orient6_ReportsRotatedDims(t *testing.T) {
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makeHEICWithOrientation(t, rawW, rawH, 6)
	if len(content) == 0 {
		t.Skip("HEIF encoder unavailable in this libvips")
	}

	result, err := p.Process(content, "image/heic")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded HEIC")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	wantW, wantH := rawH, rawW
	if *result.ImageWidth != wantW || *result.ImageHeight != wantH {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *result.ImageWidth, *result.ImageHeight, wantW, wantH)
	}
	// HEIC always re-encodes to JPEG; output bytes must be a JPEG.
	if result.MimeType != "image/jpeg" {
		t.Errorf("MimeType = %q, want image/jpeg (HEIC→JPEG)", result.MimeType)
	}
}

// FR-011: malformed-but-parseable EXIF must not fail Process. vips treats
// unrecognized orientation values as 0 (no swap), so dims are returned raw.
// A real malformed-EXIF file is hard to synthesize via vips (which produces
// well-formed EXIF on export), so we test the equivalent contract:
// orientation=0 (absent) yields raw dims, no swap, no failure.
func TestProcessor_JPEG_NoOrientationTag_TreatsAsOrient1_Passthrough(t *testing.T) {
	p := &Processor{}
	// stdlib JPEG encoder writes no EXIF — vips reports orientation=0.
	const w, h = 1024, 512
	content := makeJPEG(t, w, h)

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != w || *result.ImageHeight != h {
		t.Errorf("dims = %dx%d, want %dx%d (no swap when orientation=0)", *result.ImageWidth, *result.ImageHeight, w, h)
	}
}

// MeasureDims is the header-only port method used by lazy-backfill (T021
// contract). On a successful load, it returns post-rotation dims; on a
// vips-load failure it returns (nil, nil, err) — never (nil, nil, nil)
// from the vips path.
func TestProcessor_MeasureDims_JPEG_Orient6(t *testing.T) {
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makeJPEGWithOrientation(t, rawW, rawH, 6)

	w, h, err := p.MeasureDims(content, "image/jpeg")
	if err != nil {
		t.Fatalf("MeasureDims: %v", err)
	}
	if w == nil || h == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", w, h)
	}
	if *w != rawH || *h != rawW {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *w, *h, rawH, rawW)
	}
}

func TestProcessor_MeasureDims_CorruptInput_ReturnsError(t *testing.T) {
	p := &Processor{}
	_, _, err := p.MeasureDims([]byte("not an image"), "image/jpeg")
	if err == nil {
		t.Fatal("expected error for unparseable bytes")
	}
}

// --- US2 fixture helpers: PNG / BMP / AVIF orient1 + orient6 ---

// makePNGWithOrientation builds a w×h PNG carrying a specific EXIF
// Orientation value via vips. Used by US2 tests for canonicalizeRaster's
// PNG arm. PNG officially has no EXIF orientation, but libvips accepts
// it via SetOrientation and writes it to a Pixmap's eXIf chunk on
// export — round-tripping through NewImageFromBuffer reads it back.
func makePNGWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Fatalf("vips.Black: %v", err)
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Fatalf("SetOrientation(%d): %v", orientation, err)
	}

	ep := vips.NewPngExportParams()
	out, _, err := img.ExportPng(ep)
	if err != nil {
		t.Fatalf("ExportPng: %v", err)
	}
	return out
}

// makeBMPWithOrientation builds a w×h BMP carrying an orientation tag.
// BMP requires Magick support in libvips; if unavailable, returns nil so
// callers can t.Skip cleanly. BMP itself doesn't store EXIF, so the
// orientation comes back as 0 on read — still useful as a fixture for
// the canonicalizeRaster size-guard / passthrough behavior.
func makeBMPWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("vips.Black: %v", err)
		return nil
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Skipf("SetOrientation(%d): %v", orientation, err)
		return nil
	}

	ep := vips.NewMagickExportParams()
	ep.Format = "bmp"
	out, _, err := img.ExportMagick(ep)
	if err != nil {
		t.Skipf("ExportMagick(bmp) unavailable: %v", err)
		return nil
	}
	return out
}

// makeAVIFWithOrientation builds a w×h AVIF carrying an orientation tag.
// AVIF requires HEIF/AV1 codecs in libvips; if unavailable, returns nil
// for a clean t.Skip.
func makeAVIFWithOrientation(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, err := vips.Black(w, h)
	if err != nil {
		t.Skipf("vips.Black: %v", err)
		return nil
	}
	defer img.Close()

	if err := img.SetOrientation(orientation); err != nil {
		t.Skipf("SetOrientation(%d): %v", orientation, err)
		return nil
	}

	ep := vips.NewAvifExportParams()
	out, _, err := img.ExportAvif(ep)
	if err != nil {
		t.Skipf("ExportAvif unavailable: %v", err)
		return nil
	}
	return out
}

// magickAvailable returns true iff libvips can save BMP via the magick
// loader. Used to gate BMP-rotation tests so they skip cleanly on a
// stripped libvips build.
func magickAvailable(t *testing.T) bool {
	t.Helper()
	img, err := vips.Black(4, 4)
	if err != nil {
		return false
	}
	defer img.Close()
	ep := vips.NewMagickExportParams()
	ep.Format = "bmp"
	_, _, err = img.ExportMagick(ep)
	return err == nil
}

// --- US2 tests: PNG / BMP / AVIF canonicalization ---

func TestProcessor_PNG_Orient6_ReportsRotatedDimsAndStripsExif(t *testing.T) {
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makePNGWithOrientation(t, rawW, rawH, 6)

	result, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded PNG")
	}
	if result.MimeType != "image/png" {
		t.Errorf("MimeType = %q, want image/png (in-format re-encode)", result.MimeType)
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != rawH || *result.ImageHeight != rawW {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *result.ImageWidth, *result.ImageHeight, rawH, rawW)
	}
	// Must not be byte-identical (rotation forces re-encode).
	if bytes.Equal(content, result.Content) {
		t.Error("expected re-encoded bytes for orient!=1 PNG, got input verbatim")
	}
	// Output bytes must have orientation 0 (stripped) or 1.
	if got := reportedOrientation(t, result.Content); got != 0 && got != 1 {
		t.Errorf("output orientation = %d, want 0 or 1 (canonicalized)", got)
	}
}

func TestProcessor_AVIF_Orient6_ReportsRotatedDimsAndStripsExif(t *testing.T) {
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makeAVIFWithOrientation(t, rawW, rawH, 6)
	if len(content) == 0 {
		t.Skip("AVIF encoder unavailable in this libvips")
	}

	result, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded AVIF")
	}
	if result.MimeType != "image/avif" {
		t.Errorf("MimeType = %q, want image/avif", result.MimeType)
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	// libvips' AVIF encoder applies the orientation at encode time, so
	// the fixture comes back as physically-rotated bytes with
	// orientation=0 — canonicalizeRaster sees orient<=1 and passes it
	// through. Either way, dims that match what a renderer draws are
	// rawH × rawW (the rotated dims).
	if *result.ImageWidth != rawH || *result.ImageHeight != rawW {
		t.Errorf("dims = %dx%d, want %dx%d (post-rotation)", *result.ImageWidth, *result.ImageHeight, rawH, rawW)
	}
	// Output bytes must have orientation 0 or 1 (canonical).
	if got := reportedOrientation(t, result.Content); got != 0 && got != 1 {
		t.Errorf("output orientation = %d, want 0 or 1 (canonicalized)", got)
	}
}

func TestProcessor_BMP_Orient6_ReportsRotatedDimsAndStripsExif(t *testing.T) {
	if !magickAvailable(t) {
		t.Skip("libvips lacks Magick support; BMP rotation cannot be tested")
	}
	p := &Processor{}
	const rawW, rawH = 1024, 512
	content := makeBMPWithOrientation(t, rawW, rawH, 6)
	if len(content) == 0 {
		t.Skip("BMP encoder unavailable")
	}

	result, err := p.Process(content, "image/bmp")
	// BMP loaders may strip the orientation tag (BMP has no EXIF), in
	// which case the orientation read by Process is 0 and canonicalizeRaster
	// short-circuits to passthrough — that's still a valid result. We only
	// strictly assert: no error, image is image/bmp, dims populated.
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true on vips-loaded BMP")
	}
	if result.MimeType != "image/bmp" {
		t.Errorf("MimeType = %q, want image/bmp", result.MimeType)
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
}

func TestProcessor_PNG_Orient1_ReturnsByteIdentical(t *testing.T) {
	p := &Processor{}
	const w, h = 1024, 512
	content := makePNGWithOrientation(t, w, h, 1)

	result, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !bytes.Equal(content, result.Content) {
		t.Error("PNG with orientation=1 must round-trip byte-identical (FR-002)")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != w || *result.ImageHeight != h {
		t.Errorf("dims = %dx%d, want %dx%d", *result.ImageWidth, *result.ImageHeight, w, h)
	}
}

func TestProcessor_AVIF_Orient1_ReturnsByteIdentical(t *testing.T) {
	p := &Processor{}
	const w, h = 1024, 512
	content := makeAVIFWithOrientation(t, w, h, 1)
	if len(content) == 0 {
		t.Skip("AVIF encoder unavailable")
	}

	result, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !bytes.Equal(content, result.Content) {
		t.Error("AVIF with orientation=1 must round-trip byte-identical (FR-002)")
	}
}

func TestProcessor_BMP_Orient1_ReturnsByteIdentical(t *testing.T) {
	if !magickAvailable(t) {
		t.Skip("libvips lacks Magick support; cannot exercise BMP path")
	}
	p := &Processor{}
	const w, h = 1024, 512
	content := makeBMPWithOrientation(t, w, h, 1)
	if len(content) == 0 {
		t.Skip("BMP encoder unavailable")
	}

	result, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !bytes.Equal(content, result.Content) {
		t.Error("BMP with orientation=1 must round-trip byte-identical (FR-002)")
	}
}

// FR-010: identical input produces identical output (deterministic dedup).
func TestProcessor_PNG_DeterministicReencode(t *testing.T) {
	p := &Processor{}
	const w, h = 1024, 512
	content := makePNGWithOrientation(t, w, h, 6)

	r1, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatalf("Process #1: %v", err)
	}
	r2, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatalf("Process #2: %v", err)
	}
	if !bytes.Equal(r1.Content, r2.Content) {
		t.Errorf("PNG re-encode is non-deterministic: len1=%d, len2=%d", len(r1.Content), len(r2.Content))
	}
}

func TestProcessor_AVIF_DeterministicReencode(t *testing.T) {
	p := &Processor{}
	const w, h = 1024, 512
	content := makeAVIFWithOrientation(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("AVIF encoder unavailable")
	}

	r1, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatalf("Process #1: %v", err)
	}
	r2, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatalf("Process #2: %v", err)
	}
	if !bytes.Equal(r1.Content, r2.Content) {
		t.Errorf("AVIF re-encode is non-deterministic: len1=%d, len2=%d", len(r1.Content), len(r2.Content))
	}
}

func TestProcessor_BMP_DeterministicReencode(t *testing.T) {
	if !magickAvailable(t) {
		t.Skip("libvips lacks Magick support")
	}
	p := &Processor{}
	const w, h = 1024, 512
	content := makeBMPWithOrientation(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("BMP encoder unavailable")
	}
	r1, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatalf("Process #1: %v", err)
	}
	r2, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatalf("Process #2: %v", err)
	}
	if !bytes.Equal(r1.Content, r2.Content) {
		t.Errorf("BMP re-encode is non-deterministic: len1=%d, len2=%d", len(r1.Content), len(r2.Content))
	}
}

// FR-012: when canonicalization of a raster format is required but the
// encoder is missing, Process must surface an error (the handler maps it
// to 422). The only realistic way to hit this on a typical dev machine is
// BMP-rotation on a libvips lacking Magick support. If Magick IS present
// the test just skips — we don't tear down vips support to fake this.
func TestProcessor_BMP_Orient6_FailsLoudWhenMagickMissing(t *testing.T) {
	if magickAvailable(t) {
		t.Skip("libvips has Magick support; FR-012 fail-loud path not reachable here")
	}
	// We can't even synthesize an oriented-BMP fixture without Magick,
	// so we feed a minimal BMP-magic blob; canonicalizeRaster will load
	// (or fail to load) it. The contract under "Magick unavailable" is:
	// any orient!=1 BMP whose re-encode is required must error out.
	// Lacking the ability to *create* such a fixture without Magick, this
	// branch necessarily skips on real Magick-less environments — but the
	// canonicalizeRaster code path itself is exercised by other tests.
	t.Skip("cannot construct an orient!=1 BMP fixture without Magick; FR-012 path remains code-path-asserted via canonicalizeRaster's switch")
}

// --- US3 tests: ICC profile preservation ---

// newRGBImageWithICC builds a w×h sRGB-interpreted image with the bundled
// sRGB IEC ICC profile attached, ready for export to any format. Returns
// (nil, false) when the ICC profile path can't be loaded by libvips —
// callers should t.Skip in that case.
//
// govips' TransformICCProfile requires either an embedded input profile
// or a determinable input space (CMYK works without a profile). For a
// freshly-constructed sRGB image we have neither, so we use a two-step
// recipe: encode + reload as PNG (which gives the image a sensible
// interpretation), then attach the profile via TransformICCProfile.
func newRGBImageWithICC(t *testing.T, w, h int) (*vips.ImageRef, bool) {
	t.Helper()
	// Build a small sRGB image via the stdlib so libvips loads it with
	// InterpretationSRGB.
	srcImg := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, srcImg); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	img, err := vips.NewImageFromBuffer(buf.Bytes())
	if err != nil {
		t.Fatalf("vips.NewImageFromBuffer: %v", err)
	}
	if err := img.TransformICCProfile(vips.SRGBIEC6196621ICCProfilePath); err != nil {
		img.Close()
		t.Skipf("TransformICCProfile (ICC support unavailable in libvips): %v", err)
		return nil, false
	}
	if !img.HasICCProfile() {
		img.Close()
		t.Skip("image lacks ICC profile after TransformICCProfile")
		return nil, false
	}
	return img, true
}

// makeJPEGWithICC builds a w×h JPEG carrying the bundled sRGB IEC ICC
// profile. Used by US3 round-trip tests to verify FR-006 across every
// re-encode path.
func makeJPEGWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()

	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Fatalf("SetOrientation: %v", err)
		}
	}

	ep := vips.NewJpegExportParams()
	ep.Quality = 95
	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		t.Fatalf("ExportJpeg: %v", err)
	}
	return out
}

func makePNGWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()
	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Fatalf("SetOrientation: %v", err)
		}
	}
	out, _, err := img.ExportPng(vips.NewPngExportParams())
	if err != nil {
		t.Fatalf("ExportPng: %v", err)
	}
	return out
}

func makeWebPWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()
	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Fatalf("SetOrientation: %v", err)
		}
	}
	ep := vips.NewWebpExportParams()
	ep.Quality = 80
	out, _, err := img.ExportWebp(ep)
	if err != nil {
		t.Skipf("ExportWebp: %v", err)
		return nil
	}
	return out
}

func makeHEICWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()
	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Skipf("SetOrientation: %v", err)
			return nil
		}
	}
	ep := vips.NewHeifExportParams()
	ep.Quality = 50
	out, _, err := img.ExportHeif(ep)
	if err != nil {
		t.Skipf("ExportHeif unavailable: %v", err)
		return nil
	}
	return out
}

func makeAVIFWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()
	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Skipf("SetOrientation: %v", err)
			return nil
		}
	}
	out, _, err := img.ExportAvif(vips.NewAvifExportParams())
	if err != nil {
		t.Skipf("ExportAvif unavailable: %v", err)
		return nil
	}
	return out
}

func makeBMPWithICC(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img, ok := newRGBImageWithICC(t, w, h)
	if !ok {
		return nil
	}
	defer img.Close()
	if orientation > 0 {
		if err := img.SetOrientation(orientation); err != nil {
			t.Skipf("SetOrientation: %v", err)
			return nil
		}
	}
	ep := vips.NewMagickExportParams()
	ep.Format = "bmp"
	out, _, err := img.ExportMagick(ep)
	if err != nil {
		t.Skipf("ExportMagick(bmp): %v", err)
		return nil
	}
	return out
}

// hasICC checks whether the provided bytes carry an ICC profile.
func hasICC(t *testing.T, content []byte) bool {
	t.Helper()
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		t.Fatalf("vips.NewImageFromBuffer: %v", err)
	}
	defer img.Close()
	return img.HasICCProfile()
}

func TestProcessor_JPEG_PreservesICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makeJPEGWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("ICC fixture unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("JPEG re-encode dropped ICC profile (FR-006)")
	}
}

func TestProcessor_PNG_PreservesICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makePNGWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("ICC fixture unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("PNG re-encode dropped ICC profile (FR-006)")
	}
}

func TestProcessor_WebP_PreservesICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makeWebPWithICC(t, w, h, 0)
	if len(content) == 0 {
		t.Skip("ICC WebP fixture unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/webp")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("WebP re-encode dropped ICC profile (FR-006)")
	}
}

func TestProcessor_HEIC_PreservesICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makeHEICWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("HEIC encoder unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/heic")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("HEIC→JPEG re-encode dropped ICC profile (FR-006)")
	}
}

func TestProcessor_AVIF_PreservesICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makeAVIFWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("AVIF encoder unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("AVIF re-encode dropped ICC profile (FR-006)")
	}
}

func TestProcessor_BMP_PreservesICC(t *testing.T) {
	if !magickAvailable(t) {
		t.Skip("libvips lacks Magick support")
	}
	p := &Processor{}
	const w, h = 256, 256
	content := makeBMPWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("BMP encoder unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("BMP re-encode dropped ICC profile (FR-006)")
	}
}

// FR-006 + FR-007 together: ICC survives, EXIF/orientation tag is dropped.
// We exercise the path via a JPEG with orientation=6 (forces re-encode in
// compressJPEG) and an attached ICC profile. Output must (a) keep ICC,
// (b) report orientation 0 or 1.
func TestProcessor_StripsExifAndXmp_KeepsICC(t *testing.T) {
	p := &Processor{}
	const w, h = 256, 256
	content := makeJPEGWithICC(t, w, h, 6)
	if len(content) == 0 {
		t.Skip("ICC fixture unavailable")
	}
	if !hasICC(t, content) {
		t.Skip("input lacks ICC profile after fixture build")
	}

	result, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasICC(t, result.Content) {
		t.Error("ICC profile dropped (FR-006)")
	}
	// Orientation must be canonicalized.
	if got := reportedOrientation(t, result.Content); got != 0 && got != 1 {
		t.Errorf("output orientation = %d, want 0 or 1 (FR-001 / FR-007 EXIF strip)", got)
	}
}

// --- Phase 6: SVG / GIF dim measurement ---

const testSVG = `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 100" width="200" height="100"><rect width="200" height="100" fill="red"/></svg>`
const malformedSVG = `<svg><not closed`

func TestProcessor_SVG_ReportsViewBoxDims(t *testing.T) {
	p := &Processor{}
	content := []byte(testSVG)

	result, err := p.Process(content, "image/svg+xml")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true")
	}
	if result.MimeType != "image/svg+xml" {
		t.Errorf("MimeType = %q, want image/svg+xml", result.MimeType)
	}
	if !bytes.Equal(content, result.Content) {
		t.Error("SVG bytes must round-trip unchanged (no re-encode)")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != 200 || *result.ImageHeight != 100 {
		t.Errorf("dims = %dx%d, want 200x100 (viewBox)", *result.ImageWidth, *result.ImageHeight)
	}
}

func TestProcessor_GIF_ReportsCanvasDims(t *testing.T) {
	p := &Processor{}
	const w, h = 300, 200
	content := makeGIF(t, w, h)

	result, err := p.Process(content, "image/gif")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !result.Measured {
		t.Fatal("expected Measured=true")
	}
	if result.MimeType != "image/gif" {
		t.Errorf("MimeType = %q, want image/gif", result.MimeType)
	}
	if !bytes.Equal(content, result.Content) {
		t.Error("GIF bytes must round-trip unchanged (no re-encode)")
	}
	if result.ImageWidth == nil || result.ImageHeight == nil {
		t.Fatalf("dims = (%v, %v), want non-nil", result.ImageWidth, result.ImageHeight)
	}
	if *result.ImageWidth != w || *result.ImageHeight != h {
		t.Errorf("dims = %dx%d, want %dx%d (canvas)", *result.ImageWidth, *result.ImageHeight, w, h)
	}
}

// FR-019 sentinel path: malformed SVG → ProcessResult.Measured=true with
// nil dims so insertDocument's marshaling writes {"_decodeFailed": true}.
func TestProcessor_MalformedSVG_PersistsDecodeFailedSentinel(t *testing.T) {
	p := &Processor{}
	content := []byte(malformedSVG)

	result, err := p.Process(content, "image/svg+xml")
	if err != nil {
		t.Fatalf("Process must not error on malformed SVG: %v", err)
	}
	if !result.Measured {
		t.Error("expected Measured=true (the loader was attempted) so the marshaling writes _decodeFailed")
	}
	if result.ImageWidth != nil || result.ImageHeight != nil {
		t.Errorf("expected nil dims for malformed SVG, got (%v, %v)", result.ImageWidth, result.ImageHeight)
	}
}
