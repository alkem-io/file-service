package port

// ImageProcessor abstracts image detection and processing (MIME detection, HEIC conversion, compression).
type ImageProcessor interface {
	DetectMIME(content []byte) string
	Process(content []byte, mimeType string) (processedContent []byte, finalMIME string, err error)
}
