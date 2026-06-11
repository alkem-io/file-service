package model

import (
	"path/filepath"
	"strings"
)

// NormalizeMIME lowercases a MIME type and strips any parameters
// (e.g. "Text/Plain; charset=utf-8" → "text/plain"). Single source of
// truth for MIME comparison across the service.
func NormalizeMIME(mimeType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
}

// genericMIMEs are sniff results that carry no trustworthy type identity:
// container formats whose markers the detector missed degrade to
// application/zip, unknown binary to application/octet-stream, and
// empty/text-like bodies to text/plain. A stored type must never be
// overwritten by one of these (FR-002).
var genericMIMEs = map[string]struct{}{
	"application/zip":          {},
	"application/octet-stream": {},
	"text/plain":               {},
}

// IsGenericMIME reports whether the (raw or normalized) MIME type is one of
// the generic/ambiguous detection results.
func IsGenericMIME(mimeType string) bool {
	_, ok := genericMIMEs[NormalizeMIME(mimeType)]
	return ok
}

// OfficeExtToMIME maps office-document filename extensions to their
// canonical MIME types — the formats stored by the Collabora editing flow.
var OfficeExtToMIME = map[string]string{
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odp":  "application/vnd.oasis.opendocument.presentation",
}

// OfficeMIMEForName returns the canonical office MIME for a display name's
// extension, or ok=false when the name does not end in an office extension.
func OfficeMIMEForName(displayName string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(displayName))
	mime, ok := OfficeExtToMIME[ext]
	return mime, ok
}
