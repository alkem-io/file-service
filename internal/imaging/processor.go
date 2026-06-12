//go:build vips

package imaging

import (
	"bufio"
	"fmt"
	"io"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/gabriel-vasile/mimetype"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// Processor implements port.ImageProcessor using govips and mimetype.
type Processor struct{}

func New() *Processor {
	_ = vips.Startup(nil)
	return &Processor{}
}

func (p *Processor) DetectMIME(content []byte) string {
	return mimetype.Detect(content).String()
}

func (p *Processor) Process(content []byte, mimeType string) (port.ProcessResult, error) {
	switch mimeType {
	case "image/heic", "image/heif":
		converted, err := convertHEICToJPEG(content)
		if err != nil {
			return port.ProcessResult{}, fmt.Errorf("HEIC conversion: %w", err)
		}
		compressed, err := compressJPEG(converted)
		if err != nil {
			// Fallback: uncompressed JPEG. Re-measure dims from converted.
			w, h := measureBytes(converted)
			return port.ProcessResult{Content: converted, MimeType: "image/jpeg", ImageWidth: w, ImageHeight: h, Measured: true}, nil
		}
		return port.ProcessResult{Content: compressed.bytes, MimeType: "image/jpeg", ImageWidth: compressed.width, ImageHeight: compressed.height, Measured: true}, nil

	case "image/jpeg", "image/jpg", "image/webp":
		// Inspect orientation up front so the size-guard fallback only
		// fires when the original was already canonical. A rotated input
		// MUST round-trip as canonical bytes (FR-001) — falling back to
		// the original would re-emit the EXIF orientation tag and defeat
		// the spec.
		origOrient, origW, origH := loadHeader(content)
		compressed, err := compressJPEG(content)
		if err != nil {
			// Fallback: original bytes. Only safe when orientation is
			// already canonical (0/1); otherwise the response would
			// contain a non-canonical EXIF orientation tag.
			if origOrient <= 1 {
				return port.ProcessResult{Content: content, MimeType: mimeType, ImageWidth: origW, ImageHeight: origH, Measured: true}, nil
			}
			return port.ProcessResult{}, fmt.Errorf("compress JPEG (rotated input, no canonical fallback): %w", err)
		}
		// Size guard: if compressed is larger AND the input was already
		// canonical, keep the original to avoid quality regression.
		if len(compressed.bytes) >= len(content) && origOrient <= 1 {
			return port.ProcessResult{Content: content, MimeType: mimeType, ImageWidth: origW, ImageHeight: origH, Measured: true}, nil
		}
		finalMIME := "image/jpeg"
		if mimeType == "image/webp" {
			finalMIME = "image/jpeg" // WebP gets re-encoded to JPEG
		}
		return port.ProcessResult{Content: compressed.bytes, MimeType: finalMIME, ImageWidth: compressed.width, ImageHeight: compressed.height, Measured: true}, nil

	case "image/png", "image/bmp", "image/avif":
		return canonicalizeRaster(content, mimeType)

	case "image/svg+xml", "image/gif":
		// Pass through bytes unchanged but measure dims via header read.
		w, h := measureBytes(content)
		return port.ProcessResult{Content: content, MimeType: mimeType, ImageWidth: w, ImageHeight: h, Measured: true}, nil

	default:
		// Non-image files pass through unchanged; no measurement attempted.
		return port.ProcessResult{Content: content, MimeType: mimeType, Measured: false}, nil
	}
}

// MeasureDims is the header-only port method for the lazy-backfill path.
// MUST NOT pixel-decode or re-encode (FR-018).
func (p *Processor) MeasureDims(content []byte, _ string) (*int, *int, error) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return nil, nil, fmt.Errorf("vips load: %w", err)
	}
	defer img.Close()
	w, h := extractDims(img)
	if w == nil || h == nil {
		return nil, nil, fmt.Errorf("degenerate image dimensions")
	}
	return w, h, nil
}

// extractDims reads Width/Height/Orientation from a loaded ImageRef and
// returns post-rotation dims. EXIF orientation 5/6/7/8 swaps W and H. Returns
// (nil, nil) on degenerate (W<1 or H<1) values; the caller (Process or
// MeasureDims) decides how to surface that — Process keeps Measured=true and
// nil dims (sentinel path), MeasureDims wraps it as an error.
func extractDims(img *vips.ImageRef) (*int, *int) {
	w := img.Width()
	h := img.Height()
	if w < 1 || h < 1 {
		return nil, nil
	}
	switch img.Orientation() {
	case 5, 6, 7, 8:
		w, h = h, w
	}
	return &w, &h
}

// measureBytes is a convenience wrapper: load → extractDims → close. Returns
// (nil, nil) on any failure (caller handles via Measured flag).
func measureBytes(content []byte) (*int, *int) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return nil, nil
	}
	defer img.Close()
	return extractDims(img)
}

// loadHeader is a one-shot header read returning orientation plus
// post-rotation dims. Returns (-1, nil, nil) if vips cannot load the
// bytes; orient < 0 signals "unknown" so callers can fall back safely.
// Used by JPEG/WebP Process arms to short-circuit the size-guard
// fallback when the input is already canonical.
func loadHeader(content []byte) (orient int, width, height *int) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return -1, nil, nil
	}
	defer img.Close()
	w, h := extractDims(img)
	return img.Orientation(), w, h
}

// rasterResult bundles re-encoded bytes with their post-rotation dims so the
// JPEG/HEIC/canonicalize paths can return all three together.
type rasterResult struct {
	bytes  []byte
	width  *int
	height *int
}

func convertHEICToJPEG(content []byte) ([]byte, error) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return nil, fmt.Errorf("load HEIC: %w", err)
	}
	defer img.Close()

	if err := img.AutoRotate(); err != nil {
		return nil, fmt.Errorf("auto-rotate: %w", err)
	}

	if err := img.RemoveMetadata(); err != nil {
		return nil, fmt.Errorf("strip metadata: %w", err)
	}

	// Don't set ep.StripMetadata=true: that delegates to libvips' "strip"
	// option which strips ICC too. RemoveMetadata() above already drops
	// EXIF/IPTC/XMP while keeping ICC by govips contract (Decision 2 in
	// research.md / FR-006). Belt-and-suspenders becomes belt-only here.
	ep := vips.NewJpegExportParams()
	ep.Quality = 100 // Maximum quality for conversion

	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		return nil, fmt.Errorf("export JPEG: %w", err)
	}
	return out, nil
}

func compressJPEG(content []byte) (rasterResult, error) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return rasterResult{}, fmt.Errorf("load image: %w", err)
	}
	defer img.Close()

	if err := img.AutoRotate(); err != nil {
		return rasterResult{}, fmt.Errorf("auto-rotate: %w", err)
	}

	// Resize if longest side exceeds 4096px
	w := img.Width()
	h := img.Height()
	maxDim := 4096
	if w > maxDim || h > maxDim {
		var scale float64
		if w > h {
			scale = float64(maxDim) / float64(w)
		} else {
			scale = float64(maxDim) / float64(h)
		}
		if err := img.Resize(scale, vips.KernelLanczos3); err != nil {
			return rasterResult{}, fmt.Errorf("resize: %w", err)
		}
	}

	if err := img.RemoveMetadata(); err != nil {
		return rasterResult{}, fmt.Errorf("strip metadata: %w", err)
	}

	// See convertHEICToJPEG: not setting StripMetadata so ICC survives
	// (FR-006 / Decision 2). RemoveMetadata above dropped EXIF/IPTC/XMP.
	ep := vips.NewJpegExportParams()
	ep.Quality = 82 // MozJPEG-equivalent quality

	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		return rasterResult{}, fmt.Errorf("export JPEG: %w", err)
	}
	width, height := extractDims(img)
	return rasterResult{bytes: out, width: width, height: height}, nil
}

// canonicalizeRaster handles PNG/BMP/AVIF: header-only read for orientation;
// passthrough bytes when orient is 1 or absent; rotate + strip + re-encode in
// the same format when orient != 1. Preserves ICC via RemoveMetadata.
func canonicalizeRaster(content []byte, mimeType string) (port.ProcessResult, error) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		// vips couldn't even load — caller treats as decode failure for
		// images. Keep Measured=true so insertDocument's marshaling persists
		// _decodeFailed.
		return port.ProcessResult{Content: content, MimeType: mimeType, Measured: true}, nil
	}
	defer img.Close()

	orient := img.Orientation()
	if orient == 0 || orient == 1 {
		// Already canonical — return input bytes byte-identical.
		w, h := extractDims(img)
		return port.ProcessResult{Content: content, MimeType: mimeType, ImageWidth: w, ImageHeight: h, Measured: true}, nil
	}

	// Needs rotation: rotate, strip non-ICC metadata, re-encode in same format.
	if err := img.AutoRotate(); err != nil {
		return port.ProcessResult{}, fmt.Errorf("auto-rotate: %w", err)
	}
	if err := img.RemoveMetadata(); err != nil {
		return port.ProcessResult{}, fmt.Errorf("strip metadata: %w", err)
	}

	var out []byte
	switch mimeType {
	case "image/png":
		// StripMetadata not set: libvips' "strip" option drops ICC, which
		// FR-006 forbids. RemoveMetadata above already cleaned EXIF/IPTC/XMP
		// per Decision 2.
		ep := vips.NewPngExportParams()
		out, _, err = img.ExportPng(ep)
	case "image/avif":
		ep := vips.NewAvifExportParams()
		out, _, err = img.ExportAvif(ep)
	case "image/bmp":
		ep := vips.NewMagickExportParams()
		ep.Format = "bmp"
		// MagickExportParams has no StripMetadata field. RemoveMetadata
		// above already drops EXIF/XMP/IPTC while preserving ICC.
		out, _, err = img.ExportMagick(ep)
	default:
		return port.ProcessResult{}, fmt.Errorf("unsupported raster format for canonicalization: %s", mimeType)
	}
	if err != nil {
		// FR-012: BMP without Magick fails loudly here.
		return port.ProcessResult{}, fmt.Errorf("re-encode %s: %w", mimeType, err)
	}

	w, h := extractDims(img)
	return port.ProcessResult{Content: out, MimeType: mimeType, ImageWidth: w, ImageHeight: h, Measured: true}, nil
}

// streamHeadPeek bounds the input-header probe used for the pixel budget
// and orientation discovery (spec 020 FR-010). Part of the fixed budget.
const streamHeadPeek = 64 << 10

// TranscodeStream canonicalizes an image pulled from r and writes encoded
// chunks to w as produced (spec 020 FR-004). Strategy per the fork's
// streaming model:
//
//  1. Probe the buffered head (≤64 KiB) for dimensions + orientation
//     without consuming the stream: enforces the pixel budget before any
//     pixel decode and picks the access mode.
//  2. Orientation ≤ 2 (none / horizontal flip — sequential-safe):
//     AccessSequential, pixels flow reader→writer once, RAM bounded by
//     line caches. Orientation ≥ 3: default load, which materializes to
//     memory or scratch disc by SetStreamDiscThreshold.
//  3. RemoveMetadata (drops EXIF/IPTC/XMP, preserves ICC — Decision 2 of
//     018) → AutoRotate → SaveToWriter with streaming-capable params
//     (JPEG baseline Q82 — spec 020 R7).
func (p *Processor) TranscodeStream(r io.Reader, w io.Writer, mimeType string) (port.TranscodeResult, error) {
	br := bufio.NewReaderSize(r, streamHeadPeek)
	head, _ := br.Peek(streamHeadPeek)

	orient := -1
	if probe, err := vips.NewImageFromBuffer(head); err == nil {
		pw, ph := probe.Width(), probe.Height()
		orient = probe.Orientation()
		probe.Close()
		if budget := PixelBudget(); budget > 0 && pw > 0 && ph > 0 && int64(pw)*int64(ph) > budget {
			return port.TranscodeResult{}, fmt.Errorf("%w: %dx%d", port.ErrPixelBudgetExceeded, pw, ph)
		}
	}
	// Probe failure (header beyond the peek window) skips the pre-decode
	// budget check; decoded-frame RAM remains bounded by the disc
	// threshold on the materialized path below.

	params := vips.NewImportParams()
	if orient >= 0 && orient <= 2 {
		// Sequential fast path: rotation not needed (flip is
		// sequential-safe). Unknown orientation (probe failed) takes the
		// materialized path, which tolerates everything.
		params.Access.Set(vips.AccessSequential)
	}

	img, err := vips.LoadImageFromReader(br, params)
	if err != nil {
		return port.TranscodeResult{}, fmt.Errorf("streaming load: %w", err)
	}
	defer img.Close()

	if budget := PixelBudget(); budget > 0 && orient < 0 {
		// Probe failed earlier; the real header is now parsed — enforce
		// the budget before any pixel pass.
		if iw, ih := img.Width(), img.Height(); iw > 0 && ih > 0 && int64(iw)*int64(ih) > budget {
			return port.TranscodeResult{}, fmt.Errorf("%w: %dx%d", port.ErrPixelBudgetExceeded, iw, ih)
		}
	}

	if err := img.AutoRotate(); err != nil {
		return port.TranscodeResult{}, fmt.Errorf("auto-rotate: %w", err)
	}
	if err := img.RemoveMetadata(); err != nil {
		return port.TranscodeResult{}, fmt.Errorf("strip metadata: %w", err)
	}

	width, height := img.Width(), img.Height()

	outMIME := "image/jpeg"
	format := vips.ImageTypeJPEG
	ep := &vips.ExportParams{Quality: 82, Interlaced: false} // baseline: streaming-capable (R7)
	if mimeType == "image/png" {
		outMIME = "image/png"
		format = vips.ImageTypePNG
		ep = &vips.ExportParams{Compression: 6}
	}

	if err := img.SaveToWriter(w, format, ep); err != nil {
		return port.TranscodeResult{}, fmt.Errorf("streaming save: %w", err)
	}

	res := port.TranscodeResult{MimeType: outMIME, Measured: true}
	if width > 0 && height > 0 {
		res.ImageWidth, res.ImageHeight = &width, &height
	}
	return res, nil
}
