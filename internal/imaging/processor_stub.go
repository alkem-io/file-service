//go:build !vips

package imaging

import (
	"io"
	"net/http"

	"github.com/alkem-io/file-service/internal/domain/port"
)

// Processor is a stub implementation when libvips is not available.
// It detects MIME using Go stdlib and passes content through without image
// processing. No image decoder runs in this build, so dim measurement is
// always nil and Measured=false. The service-layer marshaling rule treats
// this as "no decoder available" and writes empty {} (not the
// _decodeFailed sentinel) so a future vips environment can retry.
type Processor struct{}

// New returns the stub processor. Unlike the vips build, no library startup
// happens here.
func New() *Processor {
	return &Processor{}
}

// DetectMIME sniffs via the stdlib's http.DetectContentType, which knows
// fewer formats than the vips build's mimetype library (e.g. HEIC comes
// back as application/octet-stream).
func (p *Processor) DetectMIME(content []byte) string {
	return http.DetectContentType(content)
}

// Process passes content through unchanged with Measured=false — no image
// processing without libvips — so insertDocument's marshaling skips the
// _decodeFailed sentinel.
func (p *Processor) Process(content []byte, mimeType string) (port.ProcessResult, error) {
	return port.ProcessResult{Content: content, MimeType: mimeType, Measured: false}, nil
}

// MeasureDims reports "no decoder available" as (nil, nil, nil);
// backfillIfNeeded skips the persist so legacy rows remain {} and a future
// vips environment can retry.
func (p *Processor) MeasureDims(_ []byte, _ string) (*int, *int, error) {
	return nil, nil, nil
}

// TranscodeStream (no-vips stub): pass-through copy, no decoding available.
// Dims unreported (Measured=false) per the lazy-backfill convention.
func (p *Processor) TranscodeStream(r io.Reader, w io.Writer, mimeType string) (port.TranscodeResult, error) {
	if _, err := io.Copy(w, r); err != nil {
		return port.TranscodeResult{}, err
	}
	return port.TranscodeResult{MimeType: mimeType, Measured: false}, nil
}
