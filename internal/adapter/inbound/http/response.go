package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"go.uber.org/zap"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeStorageReadError maps a StoragePort read failure to its HTTP response so every read
// handler answers identically: os.ErrNotExist→404 (with notFoundMsg), anything else — e.g. an NFS
// EIO/ESTALE outage, OR a malformed STORED file.externalID (a server data-integrity problem, not a
// client error) — →500 (retryable, logged with logLabel).
//
// It deliberately does NOT map port.ErrInvalidKey→400: that only applies to the by-hash endpoint,
// whose key is the CLIENT's URL {hash}, and GetBlobContent handles it there directly. Keeping the
// 400 out of the shared helper means the static openapi generator attributes it to exactly the one
// endpoint that can emit it — not to GetContent / the public serve path, which pass a stored key.
func writeStorageReadError(w http.ResponseWriter, logger *zap.Logger, err error, notFoundMsg, logLabel string) {
	if errors.Is(err, os.ErrNotExist) {
		writeJSONError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	logger.Error(logLabel, zap.Error(err))
	writeJSONError(w, http.StatusInternalServerError, "internal error")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:   http.StatusText(status),
		Message: message,
	})
}
