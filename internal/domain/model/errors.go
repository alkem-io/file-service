package model

import "errors"

// ErrDocumentNotFound is returned when a document cannot be found in the repository.
var ErrDocumentNotFound = errors.New("document not found")
