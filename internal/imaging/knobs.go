// Package imaging implements port.ImageProcessor: MIME detection, image
// canonicalization (auto-rotate, strip EXIF/IPTC/XMP while preserving ICC,
// re-encode), header-only dimension measurement, and the spec-020 streaming
// transcode. Two build variants share this package: the `vips` build tag
// selects the govips/libvips implementation; the default build is a stub
// that detects MIME via the stdlib and passes bytes through untouched.
package imaging

import "sync/atomic"

// StreamingConfig carries the spec-020 streaming knobs from service config
// into the imaging layer (and, in the vips build, into libvips).
type StreamingConfig struct {
	// DiscThreshold spills decoded frames above this many bytes to scratch
	// disc. 0 = library default (VIPS_DISC_THRESHOLD / 100 MB).
	DiscThreshold int64
	// ScratchDir hosts materialized frames. Empty = os.TempDir().
	ScratchDir string
	// PipeReadLimit bounds compressed-input accumulation for non-seekable
	// seek-needing loads. 0 = library default.
	PipeReadLimit int64
	// PixelBudget rejects images whose header-declared width×height exceeds
	// it before any pixel decode (FR-010). 0 = no budget.
	PixelBudget int64
}

var pixelBudget atomic.Int64

// PixelBudget returns the configured decode guard (0 = unlimited).
func PixelBudget() int64 { return pixelBudget.Load() }

// ConfigureStreaming applies the streaming knobs. The vips build forwards
// the library knobs to libvips (see knobs_vips.go); the stub build only
// records the pixel budget.
func ConfigureStreaming(cfg StreamingConfig) {
	pixelBudget.Store(cfg.PixelBudget)
	configureVips(cfg)
}
