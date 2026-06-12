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

func New() *Processor {
	return &Processor{}
}

func (p *Processor) DetectMIME(content []byte) string {
	return http.DetectContentType(content)
}

func (p *Processor) Process(content []byte, mimeType string) (port.ProcessResult, error) {
	// No image processing without libvips — pass through with Measured=false
	// so insertDocument's marshaling skips the _decodeFailed sentinel.
	return port.ProcessResult{Content: content, MimeType: mimeType, Measured: false}, nil
}

func (p *Processor) MeasureDims(_ []byte, _ string) (*int, *int, error) {
	// No decoder available; backfillIfNeeded skips persist on (nil, nil, nil)
	// so legacy rows remain {} and a future vips environment can retry.
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
