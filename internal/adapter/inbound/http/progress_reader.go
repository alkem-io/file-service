package http

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/alkem-io/file-service/internal/domain/service"
)

// progressReader wraps an upload body with the two streaming guards of
// spec 020: the incremental size cap (FR-005) and the progress-based idle
// timeout (FR-009 — alive while bytes arrive, aborted after `idle` without
// progress; total duration implicitly bounded by cap ÷ minimum rate).
//
// Primary mechanism: extend the connection read deadline via
// http.ResponseController before every read. Fallback (h2c transports
// where SetReadDeadline returns ErrNotSupported — analyze finding I1): a
// watchdog timer re-armed on every successful read that closes the request
// body on expiry, which unblocks the pending Read; the outcome surfaces as
// service.ErrStalled either way.
type progressReader struct {
	r         io.ReadCloser
	rc        *http.ResponseController
	idle      time.Duration
	remaining int64
	deadline  bool // SetReadDeadline supported on this transport

	mu       sync.Mutex
	watchdog *time.Timer
	stalled  bool
	closed   bool
}

func newProgressReader(w http.ResponseWriter, body io.ReadCloser, capBytes int64, idle time.Duration) *progressReader {
	pr := &progressReader{
		r:         body,
		rc:        http.NewResponseController(w),
		idle:      idle,
		remaining: capBytes,
	}
	// Probe deadline support once. ErrNotSupported → watchdog fallback.
	if err := pr.rc.SetReadDeadline(time.Now().Add(idle)); err == nil {
		pr.deadline = true
	} else if errors.Is(err, http.ErrNotSupported) {
		pr.armWatchdog()
	}
	return pr
}

func (pr *progressReader) armWatchdog() {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.closed {
		return
	}
	if pr.watchdog != nil {
		pr.watchdog.Stop()
	}
	pr.watchdog = time.AfterFunc(pr.idle, func() {
		pr.mu.Lock()
		pr.stalled = true
		pr.mu.Unlock()
		_ = pr.r.Close() // unblocks the pending Read on h2c
	})
}

func (pr *progressReader) Read(p []byte) (int, error) {
	if pr.remaining <= 0 {
		return 0, service.ErrOverLimit
	}
	if int64(len(p)) > pr.remaining+1 {
		// Allow exactly one byte over the cap through the limiter so the
		// over-limit condition is detectable (io.LimitReader idiom).
		p = p[:pr.remaining+1]
	}

	if pr.deadline {
		_ = pr.rc.SetReadDeadline(time.Now().Add(pr.idle))
	}
	n, err := pr.r.Read(p)
	pr.remaining -= int64(n)
	if !pr.deadline && n > 0 {
		pr.armWatchdog()
	}

	if pr.remaining < 0 {
		return n, service.ErrOverLimit
	}
	if err != nil && !errors.Is(err, io.EOF) {
		pr.mu.Lock()
		stalled := pr.stalled
		pr.mu.Unlock()
		if stalled || isTimeoutErr(err) {
			return n, fmt.Errorf("%w: %w", service.ErrStalled, err)
		}
	}
	return n, err
}

// isTimeoutErr matches read-deadline expiry across transports: HTTP/1
// surfaces a net.Error with Timeout()=true; the x/net h2c path can surface
// a bare "i/o timeout" without the Timeout method (hence the string check).
func isTimeoutErr(err error) bool {
	if os.IsTimeout(err) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return err.Error() == "i/o timeout"
}

// Close releases the guards: stops the watchdog and clears the connection
// read deadline so a keep-alive connection's next request starts fresh.
func (pr *progressReader) Close() error {
	pr.mu.Lock()
	pr.closed = true
	if pr.watchdog != nil {
		pr.watchdog.Stop()
	}
	pr.mu.Unlock()
	if pr.deadline {
		_ = pr.rc.SetReadDeadline(time.Time{})
	}
	return nil
}
