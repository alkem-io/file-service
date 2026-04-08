//go:build vips

package imaging

import (
	"fmt"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/gabriel-vasile/mimetype"
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

func (p *Processor) Process(content []byte, mimeType string) ([]byte, string, error) {
	switch mimeType {
	case "image/heic", "image/heif":
		converted, err := convertHEICToJPEG(content)
		if err != nil {
			return nil, "", fmt.Errorf("HEIC conversion: %w", err)
		}
		compressed, err := compressJPEG(converted)
		if err != nil {
			return converted, "image/jpeg", nil // fallback to uncompressed JPEG
		}
		return compressed, "image/jpeg", nil

	case "image/jpeg", "image/jpg", "image/webp":
		compressed, err := compressJPEG(content)
		if err != nil {
			return content, mimeType, nil // fallback to original
		}
		// Size guard: if compressed is larger, keep original
		if len(compressed) >= len(content) {
			return content, mimeType, nil
		}
		finalMIME := "image/jpeg"
		if mimeType == "image/webp" {
			finalMIME = "image/jpeg" // WebP gets re-encoded to JPEG
		}
		return compressed, finalMIME, nil

	case "image/svg+xml", "image/gif", "image/png", "image/bmp", "image/avif":
		// Pass through — don't re-compress these formats
		return content, mimeType, nil

	default:
		// Non-image files pass through unchanged
		return content, mimeType, nil
	}
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

	ep := vips.NewJpegExportParams()
	ep.Quality = 100 // Maximum quality for conversion
	ep.StripMetadata = true

	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		return nil, fmt.Errorf("export JPEG: %w", err)
	}
	return out, nil
}

func compressJPEG(content []byte) ([]byte, error) {
	img, err := vips.NewImageFromBuffer(content)
	if err != nil {
		return nil, fmt.Errorf("load image: %w", err)
	}
	defer img.Close()

	if err := img.AutoRotate(); err != nil {
		return nil, fmt.Errorf("auto-rotate: %w", err)
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
			return nil, fmt.Errorf("resize: %w", err)
		}
	}

	ep := vips.NewJpegExportParams()
	ep.Quality = 82 // MozJPEG-equivalent quality
	ep.StripMetadata = true

	out, _, err := img.ExportJpeg(ep)
	if err != nil {
		return nil, fmt.Errorf("export JPEG: %w", err)
	}
	return out, nil
}
