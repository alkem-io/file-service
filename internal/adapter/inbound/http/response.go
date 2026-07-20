package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/port"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeStorageReadError maps a StoragePort read failure to its HTTP response so every read
// handler answers identically for the same backend failure: os.ErrNotExist→404 (with
// notFoundMsg), anything else — e.g. an NFS EIO/ESTALE outage — →500 (retryable, logged with
// logLabel).
//
// keyFromClient decides ONLY how a malformed key (port.ErrInvalidKey) is reported, and it matters:
//   - true  (the by-hash endpoint, where the key is the client's URL {hash}) → 400 "invalid
//     content hash": the client sent a bad key.
//   - false (the by-id / public serve paths, where the key is the STORED file.externalID) → a
//     malformed stored key is a SERVER data-integrity problem, not a client error: 500 + a log
//     line, never a 400 that blames the caller for an unservable row.
func writeStorageReadError(w http.ResponseWriter, logger *zap.Logger, err error, notFoundMsg, logLabel string, keyFromClient bool) {
	switch {
	case keyFromClient && errors.Is(err, port.ErrInvalidKey):
		writeJSONError(w, http.StatusBadRequest, "invalid content hash")
	case errors.Is(err, os.ErrNotExist):
		writeJSONError(w, http.StatusNotFound, notFoundMsg)
	default:
		logger.Error(logLabel, zap.Error(err))
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:   http.StatusText(status),
		Message: message,
	})
}
