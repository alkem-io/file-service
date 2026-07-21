package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"go.uber.org/zap"
)

// streamBlob copies a storage blob to w in CONSTANT MEMORY (never buffering the whole object into
// a []byte), AFTER the caller has written the status line + Content-Length. It takes ownership of
// rc and closes it. HTTP cannot retract the already-sent 200, so a mid-stream failure is
// classified — which io.Copy alone conflates — so a truncated body can't pass for a clean success
// against the declared Content-Length:
//   - a BACKEND read fault (rc.Read errors, e.g. an NFS EIO/ESTALE): a genuine truncation → WARN +
//     panic(http.ErrAbortHandler) so the connection aborts abnormally rather than looking like EOF.
//   - a SHORT read (clean io.EOF before `size` — the backing file shrank after Stat, via a
//     concurrent refcount cleanup/replace): io.Copy reports (written<size, nil), so it bypasses the
//     read wrapper — caught here and made equally loud.
//   - a CLIENT write failure (a routine consumer cancel/timeout/disconnect): benign — the response
//     is moot; stay silent (a WARN would be alert-fatigue noise masking the real EIO signal).
func streamBlob(w http.ResponseWriter, logger *zap.Logger, rc io.ReadCloser, size int64, key string) {
	defer func() { _ = rc.Close() }()
	src := &readErrCapture{r: rc}
	written, err := io.Copy(w, src)
	switch {
	case err != nil && src.err != nil:
		logger.Warn("blob stream truncated by a backend read fault — aborting connection",
			zap.String("key", key), zap.Error(src.err))
		panic(http.ErrAbortHandler)
	case err != nil:
		// client write failure — the caller went away. Benign; nothing to do.
	case written < size:
		logger.Warn("blob stream truncated (short read: EOF before the declared size) — aborting connection",
			zap.String("key", key), zap.Int64("written", written), zap.Int64("size", size))
		panic(http.ErrAbortHandler)
	}
}

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
