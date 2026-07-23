package port

import (
	"errors"
	"io"
)

// ProcessResult captures everything Process decided about an upload:
// the post-canonicalization bytes, the (possibly re-detected) MIME, optional
// post-rotation dimensions for image MIMEs, and a flag indicating whether
// an image decoder actually attempted to read the bytes.
//
// Measured distinguishes "decoder ran" from "no decoder available":
//   - Measured=true  → the image decoder ran on these bytes; if dims are nil
//     the decoder confirmed the bytes are unreadable (the marshaling rule in
//     service.insertDocument writes the {"_decodeFailed": true} sentinel).
//   - Measured=false → no decoder attempted (no-vips stub for the !vips
//     build); nil dims mean nothing about the bytes (the marshaling rule
//     writes empty {} so a future vips run can retry).
type ProcessResult struct {
	Content     []byte
	MimeType    string
	ImageWidth  *int
	ImageHeight *int
	Measured    bool
}

// ErrPixelBudgetExceeded rejects an image whose header-declared dimensions
// exceed the configured pixel budget — checked before any pixel decode
// (spec 020 FR-010). The budget is the only RAM guard for whole-frame
// codecs (HEIC) and defends CPU/scratch-disk for every format.
var ErrPixelBudgetExceeded = errors.New("image dimensions exceed the configured pixel budget")

// TranscodeResult reports a streaming transcode's outcome (spec 020): the
// final stored MIME and the header-derived, orientation-corrected pixel
// dimensions of the output. Dims follow the ProcessResult convention:
// nil+Measured=false from the no-vips stub, nil+Measured=true for a decoder
// that ran but could not measure.
type TranscodeResult struct {
	MimeType    string
	ImageWidth  *int
	ImageHeight *int
	Measured    bool
}

// ImageProcessor abstracts image detection, canonicalization, and dimension
// extraction.
type ImageProcessor interface {
	// DetectMIME sniffs the MIME type from the content bytes alone. Never
	// fails: undetectable content comes back as a generic type (e.g.
	// application/octet-stream). The vips build uses the mimetype library;
	// the stub falls back to http.DetectContentType, which knows fewer
	// formats — callers must not assume the two builds agree.
	DetectMIME(content []byte) string

	// TranscodeStream canonicalizes an image pulled from r and writes the
	// encoded output to w in chunks as produced (spec 020 FR-004). The
	// compressed input and output are never held whole in service memory;
	// decoded-frame residency is governed by the library's disc threshold,
	// except for whole-frame codecs (HEIC) which the caller bounds via the
	// pixel budget before invoking. The stub build copies r to w unchanged.
	TranscodeStream(r io.Reader, w io.Writer, mimeType string) (TranscodeResult, error)

	// Process canonicalizes image bytes (auto-rotate, strip EXIF/IPTC/XMP
	// while preserving ICC, optionally re-encode) and returns the processed
	// bytes plus dims. May pixel-decode and re-encode for raster
	// orientation!=1 inputs; not suitable for legacy backfill (which must
	// be header-only — see MeasureDims).
	Process(content []byte, mimeType string) (ProcessResult, error)

	// MeasureDims performs a header-only decode and returns post-rotation
	// pixel dimensions. MUST NOT pixel-decode or re-encode (FR-018: "no
	// pixel decode, microseconds even for large files"). Used by the
	// explicit sweep-dims job for legacy rows whose content_metadata is
	// empty.
	//
	// Contract:
	//   - (dims, nil)        → success; both ImageWidth and ImageHeight non-nil.
	//   - (nil, nil, err)    → decoder ran and failed (corrupt bytes,
	//                          unsupported codec, degenerate W/H values).
	//                          Caller persists {"_decodeFailed": true}.
	//   - (nil, nil, nil)    → only emitted by the no-vips stub; signals
	//                          "no decoder available." Caller skips persist
	//                          so a future vips run can retry.
	MeasureDims(r io.Reader, mimeType string) (*int, *int, error)
}
