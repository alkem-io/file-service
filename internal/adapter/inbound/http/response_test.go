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
	header               http.Header
	deadlines            []time.Time
	readDeadlines        []time.Time
	flushes              int
	deadlineCallsAtFlush int
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

func (w *deadlineResponseWriter) FlushError() error {
	w.flushes++
	w.deadlineCallsAtFlush = len(w.deadlines)
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

func TestResponseWriteDeadline_ArmsLazilyRollsFlushesAndClears(t *testing.T) {
	w := &deadlineResponseWriter{}
	handler := responseWriteDeadline(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if len(w.deadlines) != 0 {
			t.Fatalf("deadline armed while reading request body: %v", w.deadlines)
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte("body"))
	}))

	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request body")))

	if len(w.deadlines) != 4 {
		t.Fatalf("deadline calls = %d, want header + body + final-flush refresh + clear", len(w.deadlines))
	}
	if w.deadlines[0].IsZero() || w.deadlines[1].IsZero() || w.deadlines[2].IsZero() {
		t.Fatalf("response deadlines were not armed: %v", w.deadlines)
	}
	if w.flushes != 1 || w.deadlineCallsAtFlush != 3 {
		t.Fatalf("flushes=%d deadlineCallsAtFlush=%d, want one flush after final refresh", w.flushes, w.deadlineCallsAtFlush)
	}
	if !w.deadlines[3].IsZero() {
		t.Fatalf("final deadline = %v, want zero-time clear", w.deadlines[3])
	}
}

func TestResponseWriteDeadline_BoundsImplicitFinalResponse(t *testing.T) {
	w := &deadlineResponseWriter{}
	handler := responseWriteDeadline(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(w.deadlines) != 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
		t.Fatalf("deadlines = %v, want final-flush arm then clear", w.deadlines)
	}
	if w.flushes != 1 || w.deadlineCallsAtFlush != 1 {
		t.Fatalf("flushes=%d deadlineCallsAtFlush=%d, want bounded implicit flush", w.flushes, w.deadlineCallsAtFlush)
	}
}

func TestResponseWriteDeadline_DoesNotFlushWhilePanicking(t *testing.T) {
	w := &deadlineResponseWriter{}
	handler := responseWriteDeadline(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	defer func() {
		if recover() == nil {
			t.Fatal("expected downstream panic to propagate")
		}
		if w.flushes != 0 || len(w.deadlines) != 0 {
			t.Fatalf("panic unwind flushed=%d deadlines=%v, want no response commit", w.flushes, w.deadlines)
		}
	}()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestWriteIdleGuard_ReachesTransportThroughStatusWriter(t *testing.T) {
	transport := &deadlineResponseWriter{}
	w := &statusWriter{ResponseWriter: transport}
	g := newWriteIdleGuard(w, time.Second)
	g.close()

	if len(transport.deadlines) != 2 || transport.deadlines[0].IsZero() || !transport.deadlines[1].IsZero() {
		t.Fatalf("deadlines through statusWriter = %v, want arm then clear", transport.deadlines)
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
