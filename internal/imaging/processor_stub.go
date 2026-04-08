//go:build !vips

package imaging

import "net/http"

// Processor is a stub implementation when libvips is not available.
// It detects MIME using Go stdlib and passes content through without image processing.
type Processor struct{}

func New() *Processor {
	return &Processor{}
}

func (p *Processor) DetectMIME(content []byte) string {
	return http.DetectContentType(content)
}

func (p *Processor) Process(content []byte, mimeType string) ([]byte, string, error) {
	// No image processing without libvips — pass through
	return content, mimeType, nil
}
