package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

const responseWriteIdleTimeout = 30 * time.Second

// responseWriteDeadline applies a rolling write-idle deadline to every response. It arms lazily on
// the first WriteHeader/Write, so a long upload is never constrained while its request body is
// still being read. Small JSON responses get a bounded write path, while streamed blobs refresh
// the same idle window on every chunk instead of inheriting an absolute request-duration limit.
func responseWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &deadlineWriter{
			ResponseWriter: w,
			controller:     http.NewResponseController(w),
			idle:           responseWriteIdleTimeout,
		}
		next.ServeHTTP(dw, r)
		// Only flush after a normal return. If next panics, unwinding must reach the outer recovery
		// middleware before anything commits net/http's implicit 200 response.
		dw.finish()
	})
}

type deadlineWriter struct {
	http.ResponseWriter
	controller  *http.ResponseController
	idle        time.Duration
	armed       bool
	unsupported bool
}

// WriteHeader refreshes the idle deadline before sending response headers.
func (w *deadlineWriter) WriteHeader(code int) {
	w.refresh()
	w.ResponseWriter.WriteHeader(code)
}

// Write refreshes the idle deadline before sending response body bytes.
func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.refresh()
	return w.ResponseWriter.Write(p)
}

// Unwrap lets nested ResponseControllers (notably streamBlob's explicit guard) reach the transport.
func (w *deadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *deadlineWriter) refresh() {
	if w.unsupported {
		return
	}
	if err := w.controller.SetWriteDeadline(time.Now().Add(w.idle)); err != nil {
		w.unsupported = true
		return
	}
	w.armed = true
}

// finish gives net/http's buffered response a bounded transport flush before clearing the
// connection deadline. ServeHTTP returning is not itself the final write: net/http normally flushes
// its response buffer afterwards, so clearing first would leave that write unbounded. Refreshing
// here also covers handlers that return without explicitly calling WriteHeader or Write.
func (w *deadlineWriter) finish() {
	w.refresh()
	_ = w.controller.Flush()
	if w.armed {
		_ = w.controller.SetWriteDeadline(time.Time{})
	}
}

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
	guard := newWriteIdleGuard(w, responseWriteIdleTimeout)
	defer guard.close()
	// Never write beyond the Content-Length captured from the opened handle.
	// A corrupted/mutated backing file that grows after Stat therefore cannot
	// spill an extra byte into HTTP framing and masquerade as a client-write
	// failure ("wrote more than the declared Content-Length").
	limited := io.LimitReader(src, size)
	// Keep the real ResponseWriter as io.Copy's destination (also lets the
	// OpenAPI generator recognize a binary response). The source wrapper
	// refreshes the write deadline after each successful read, immediately
	// before io.Copy writes that chunk.
	guarded := &writeDeadlineReader{r: limited, guard: guard}
	written, err := io.Copy(w, guarded)
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

// writeIdleGuard replaces net/http.Server.WriteTimeout's absolute
// header-to-finish deadline with progress semantics suitable for large streamed
// bodies: each write gets a fresh idle window. If the transport cannot expose
// deadlines, writes still proceed; client cancellation remains visible through
// ResponseWriter.Write.
type writeIdleGuard struct {
	rc        *http.ResponseController
	idle      time.Duration
	supported bool
}

func newWriteIdleGuard(w http.ResponseWriter, idle time.Duration) *writeIdleGuard {
	g := &writeIdleGuard{rc: http.NewResponseController(w), idle: idle}
	if err := g.rc.SetWriteDeadline(time.Now().Add(idle)); err == nil {
		g.supported = true
	}
	return g
}

func (g *writeIdleGuard) refresh() {
	if g.supported {
		_ = g.rc.SetWriteDeadline(time.Now().Add(g.idle))
	}
}

func (g *writeIdleGuard) close() {
	if g.supported {
		_ = g.rc.SetWriteDeadline(time.Time{})
	}
}

type writeDeadlineReader struct {
	r     io.Reader
	guard *writeIdleGuard
}

func (r *writeDeadlineReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.guard.refresh()
	}
	return n, err
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{
		Error:   http.StatusText(status),
		Message: message,
	})
}
