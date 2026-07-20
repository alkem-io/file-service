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
// handler (blob-by-hash, file-by-id, public serve) answers identically for the same backend
// failure: port.ErrInvalidKey→400, os.ErrNotExist→404 (with notFoundMsg), anything else — e.g.
// an NFS EIO/ESTALE outage — →500 (retryable). logLabel names the failure in the 500 server log.
func writeStorageReadError(w http.ResponseWriter, logger *zap.Logger, err error, notFoundMsg, logLabel string) {
	switch {
	case errors.Is(err, port.ErrInvalidKey):
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
