package http

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// referenceQuery validates one bounded opaque reference without rewriting it.
func referenceQuery(r *http.Request) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("malformed query string")
	}
	if len(q["ref"]) != 1 || q.Get("ref") == "" {
		return nil, fmt.Errorf("one non-empty ref is required")
	}
	ref := q.Get("ref")
	if err = validateExternalReference(&ref); err != nil {
		return nil, err
	}
	return q, nil
}

func handleReferenceLookupFailure(w http.ResponseWriter, logger *zap.Logger, err error) {
	if errors.Is(err, model.ErrDocumentNotFound) {
		writeJSONError(w, http.StatusNotFound, "document not found")
		return
	}
	logger.Error("failed to lookup document by reference", zap.Error(err))
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

// serveDocumentBlob is shared by the authorized document route and both reference
// routes after lookup/authorization. It never returns a row or redirects to an ID.
func serveDocumentBlob(w http.ResponseWriter, r *http.Request, doc model.Document, storage port.StoragePort, logger *zap.Logger, maxAge int) {
	etag := `"` + doc.ExternalID + `"`
	if r.Header.Get("If-None-Match") == etag {
		applyServeHardening(w, doc)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, size, err := storage.ReadStream(doc.ExternalID)
	if err != nil {
		writeStorageReadError(w, logger, err, "file not found on storage", "failed to read file from storage")
		return
	}
	w.Header().Set("Content-Type", doc.MimeType)
	applyServeHardening(w, doc)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	w.Header().Set("Pragma", "public")
	w.Header().Set("Expires", time.Now().Add(time.Duration(maxAge)*time.Second).UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", etag)
	if doc.ImageWidth != nil {
		w.Header().Set("X-Alkemio-Image-Width", strconv.Itoa(*doc.ImageWidth))
	}
	if doc.ImageHeight != nil {
		w.Header().Set("X-Alkemio-Image-Height", strconv.Itoa(*doc.ImageHeight))
	}
	if rs, ok := rc.(io.ReadSeeker); ok {
		defer func() { _ = rc.Close() }()
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		_ = rc.Close()
		return
	}
	streamBlob(w, logger, rc, size, doc.ExternalID)
}
