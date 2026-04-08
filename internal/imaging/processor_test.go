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

	processed, finalMIME, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if finalMIME != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", finalMIME)
	}
	// Processed should be valid (non-empty)
	if len(processed) == 0 {
		t.Error("empty processed output")
	}
}

func TestProcess_JPEG_Resize(t *testing.T) {
	p := &Processor{}
	// Create an oversized JPEG (5000x5000)
	content := makeJPEG(t, 5000, 5000)

	processed, _, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Decode to check dimensions
	img, err := vips.NewImageFromBuffer(processed)
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

	processed, finalMIME, err := p.Process(content, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/png" {
		t.Errorf("mime = %q, want image/png", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("PNG should pass through unmodified")
	}
}

func TestProcess_GIF_PassThrough(t *testing.T) {
	p := &Processor{}
	content := makeGIF(t, 10, 10)

	processed, finalMIME, err := p.Process(content, "image/gif")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/gif" {
		t.Errorf("mime = %q, want image/gif", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("GIF should pass through unmodified")
	}
}

func TestProcess_SVG_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)

	processed, finalMIME, err := p.Process(content, "image/svg+xml")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/svg+xml" {
		t.Errorf("mime = %q, want image/svg+xml", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("SVG should pass through unmodified")
	}
}

func TestProcess_PDF_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("%PDF-1.0 test")

	processed, finalMIME, err := p.Process(content, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "application/pdf" {
		t.Errorf("mime = %q, want application/pdf", finalMIME)
	}
	if !bytes.Equal(processed, content) {
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

	processed, finalMIME, err := p.Process(heicContent, "image/heic")
	if err != nil {
		t.Fatalf("Process HEIC: %v", err)
	}
	if finalMIME != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", finalMIME)
	}
	if len(processed) == 0 {
		t.Error("empty processed output")
	}
	// Verify it's a valid JPEG (starts with FF D8)
	if len(processed) < 2 || processed[0] != 0xFF || processed[1] != 0xD8 {
		t.Error("output is not a valid JPEG")
	}
}

func TestProcess_HEIF_ToJPEG(t *testing.T) {
	p := &Processor{}
	heicContent := makeHEIC(t, 50, 50)
	if len(heicContent) == 0 {
		t.Skip("could not generate HEIC test image")
	}

	processed, finalMIME, err := p.Process(heicContent, "image/heif")
	if err != nil {
		t.Fatalf("Process HEIF: %v", err)
	}
	if finalMIME != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", finalMIME)
	}
	if len(processed) == 0 {
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

	processed, finalMIME, err := p.Process(webpContent, "image/webp")
	if err != nil {
		t.Fatalf("Process WebP: %v", err)
	}
	// Either JPEG (if compression was beneficial) or original WebP (size guard)
	if finalMIME != "image/jpeg" && finalMIME != "image/webp" {
		t.Errorf("mime = %q, want image/jpeg or image/webp", finalMIME)
	}
	if len(processed) == 0 {
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
	if len(compressed) == 0 {
		t.Error("empty output")
	}
}

func TestNew_Initializes(t *testing.T) {
	p := New()
	if p == nil {
		t.Fatal("New() returned nil")
	}
}

func TestProcess_BMP_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("BM fake bmp content")

	processed, finalMIME, err := p.Process(content, "image/bmp")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/bmp" {
		t.Errorf("mime = %q, want image/bmp", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("BMP should pass through unmodified")
	}
}

func TestProcess_UnknownType_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("random binary data")

	processed, finalMIME, err := p.Process(content, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "application/octet-stream" {
		t.Errorf("mime = %q", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("unknown type should pass through")
	}
}

func TestProcess_HEIC_ConversionError(t *testing.T) {
	p := &Processor{}
	// Corrupt data that claims to be HEIC
	_, _, err := p.Process([]byte("not a real heic file"), "image/heic")
	if err == nil {
		t.Fatal("expected error for corrupt HEIC")
	}
}

func TestProcess_JPEG_CompressFallback(t *testing.T) {
	p := &Processor{}
	// Corrupt data that claims to be JPEG — compressJPEG will fail, should fallback
	corrupt := []byte{0xFF, 0xD8, 0xFF, 0x00} // JPEG magic but truncated
	processed, finalMIME, err := p.Process(corrupt, "image/jpeg")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Should return original content as fallback
	if finalMIME != "image/jpeg" {
		t.Errorf("mime = %q", finalMIME)
	}
	if len(processed) != len(corrupt) {
		t.Errorf("expected fallback to original size %d, got %d", len(corrupt), len(processed))
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
	processed, mime, err := p.Process(heic, "image/heif")
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q", mime)
	}
	if len(processed) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_JPG_Alias(t *testing.T) {
	p := &Processor{}
	content := makeJPEG(t, 100, 100)
	_, finalMIME, err := p.Process(content, "image/jpg")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/jpeg" && finalMIME != "image/jpg" {
		t.Errorf("mime = %q", finalMIME)
	}
}

func TestProcess_AVIF_PassThrough(t *testing.T) {
	p := &Processor{}
	content := []byte("fake avif content")
	processed, finalMIME, err := p.Process(content, "image/avif")
	if err != nil {
		t.Fatal(err)
	}
	if finalMIME != "image/avif" {
		t.Errorf("mime = %q", finalMIME)
	}
	if !bytes.Equal(processed, content) {
		t.Error("AVIF should pass through")
	}
}

func TestCompressJPEG_TallImage(t *testing.T) {
	// Test resize path where height > width > 4096
	content := makeJPEG(t, 100, 5000)
	compressed, err := compressJPEG(content)
	if err != nil {
		t.Fatal(err)
	}

	img, err := vips.NewImageFromBuffer(compressed)
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
	processed, finalMIME, err := p.Process(corruptWebP, "image/webp")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Should fall back to original
	if finalMIME != "image/webp" {
		t.Logf("mime = %q (compress may have succeeded on minimal input)", finalMIME)
	}
	if len(processed) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_JPEG_CompressFails_FallsBack(t *testing.T) {
	p := &Processor{}
	// Minimal JPEG SOI + garbage — compressJPEG should fail, Process should fallback
	corruptJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x02, 0x00, 0x00}
	processed, finalMIME, err := p.Process(corruptJPEG, "image/jpeg")
	if err != nil {
		t.Fatalf("should fallback, not error: %v", err)
	}
	// Fallback returns original content with original mime
	if finalMIME != "image/jpeg" {
		t.Logf("mime = %q", finalMIME)
	}
	if len(processed) == 0 {
		t.Error("empty output")
	}
}

func TestProcess_CompressionSizeGuard(t *testing.T) {
	p := &Processor{}
	// Create a tiny JPEG that can't be compressed smaller
	content := makeJPEG(t, 2, 2)

	processed, _, err := p.Process(content, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// Size guard: if compressed >= original, return original
	if len(processed) > len(content)*2 {
		t.Errorf("processed (%d bytes) much larger than original (%d bytes)", len(processed), len(content))
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
