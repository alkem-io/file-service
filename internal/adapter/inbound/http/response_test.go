package http

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

type deadlineResponseWriter struct {
	header        http.Header
	deadlines     []time.Time
	readDeadlines []time.Time
}

func (w *deadlineResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *deadlineResponseWriter) WriteHeader(int)             {}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineResponseWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadlines = append(w.readDeadlines, deadline)
	return nil
}

func TestWriteIdleGuard_RollsDeadlineAndClearsIt(t *testing.T) {
	w := &deadlineResponseWriter{}
	g := newWriteIdleGuard(w, time.Second)

	g.refresh()
	g.refresh()
	g.close()

	// One probe/arm, one refresh per write, then the zero-time clear.
	if len(w.deadlines) != 4 {
		t.Fatalf("deadline calls = %d, want 4", len(w.deadlines))
	}
	for i, deadline := range w.deadlines[:3] {
		if deadline.IsZero() {
			t.Fatalf("deadline call %d was zero before Close", i)
		}
	}
	if !w.deadlines[3].IsZero() {
		t.Fatalf("Close deadline = %v, want zero-time clear", w.deadlines[3])
	}
}

func TestStreamBlob_NeverWritesPastDeclaredSize(t *testing.T) {
	w := httptest.NewRecorder()
	streamBlob(w, zap.NewNop(), io.NopCloser(strings.NewReader("abcdef")), 3, "hash")
	if got := w.Body.String(); got != "abc" {
		t.Fatalf("body = %q, want exactly the declared 3 bytes", got)
	}
}

func TestWriteJSONErrorDeclaresJSONContentType(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSONError(w, http.StatusBadRequest, "bad input")

	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JSON with UTF-8 charset", got)
	}
}
