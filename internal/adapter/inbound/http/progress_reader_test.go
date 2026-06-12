package http

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/alkem-io/file-service/internal/domain/service"
)

// Spec 020 T013 — progressReader guards under BOTH transports the server
// runs: HTTP/1 and h2c (cmd/server/app.go). Analyze finding I1.

// stallResult captures what the server-side read loop observed.
type stallResult struct {
	mu  sync.Mutex
	err error
	n   int64
}

func (sr *stallResult) set(n int64, err error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.n, sr.err = n, err
}

func (sr *stallResult) get() (int64, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.n, sr.err
}

func drainHandler(capBytes int64, idle time.Duration, res *stallResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := newProgressReader(w, r.Body, capBytes, idle)
		defer func() { _ = pr.Close() }()
		buf := make([]byte, 4096)
		var total int64
		for {
			n, err := pr.Read(buf)
			total += int64(n)
			if err != nil {
				if errors.Is(err, io.EOF) {
					res.set(total, nil)
				} else {
					res.set(total, err)
				}
				break
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}

// slowBody writes an initial burst then stalls forever (until closed).
type slowBody struct {
	burst   []byte
	sent    bool
	unstuck chan struct{}
}

func (s *slowBody) Read(p []byte) (int, error) {
	if !s.sent {
		s.sent = true
		n := copy(p, s.burst)
		return n, nil
	}
	<-s.unstuck // stall: no more bytes until the test releases
	return 0, io.EOF
}

func runStallScenario(t *testing.T, srv *httptest.Server, client *http.Client, res *stallResult) {
	t.Helper()

	body := &slowBody{burst: []byte("initial burst of upload bytes"), unstuck: make(chan struct{})}
	defer close(body.unstuck)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(body))
	req.ContentLength = -1 // streamed

	done := make(chan struct{})
	go func() {
		resp, err := client.Do(req)
		_ = err
		if resp != nil {
			_ = resp.Body.Close()
		}
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			n, err := res.get()
			t.Fatalf("server never observed the stall: n=%d err=%v", n, err)
		case <-time.After(50 * time.Millisecond):
			if _, err := res.get(); err != nil {
				if n, err := res.get(); !errors.Is(err, service.ErrStalled) {
					t.Fatalf("read error = %v (n=%d), want ErrStalled", err, n)
				}
				<-done
				return
			}
		}
	}
}

func TestProgressReader_StallAborts_HTTP1(t *testing.T) {
	res := &stallResult{}
	srv := httptest.NewServer(drainHandler(1<<20, 250*time.Millisecond, res))
	defer srv.Close()
	runStallScenario(t, srv, srv.Client(), res)
}

func TestProgressReader_StallAborts_H2C(t *testing.T) {
	res := &stallResult{}
	srv := httptest.NewServer(h2c.NewHandler(drainHandler(1<<20, 250*time.Millisecond, res), &http2.Server{}))
	defer srv.Close()

	client := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	runStallScenario(t, srv, client, res)
}

func TestProgressReader_OverLimit(t *testing.T) {
	res := &stallResult{}
	srv := httptest.NewServer(drainHandler(1024, time.Second, res))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(io.LimitReader(neverEnding('x'), 64<<10)))
	req.ContentLength = -1
	resp, err := srv.Client().Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}

	waitFor(t, func() bool { _, e := res.get(); return e != nil })
	if n, err := res.get(); !errors.Is(err, service.ErrOverLimit) {
		t.Fatalf("read error = %v (n=%d), want ErrOverLimit", err, n)
	}
	if n, _ := res.get(); n > 1025 {
		t.Errorf("server consumed %d bytes past a 1024 cap", n)
	}
}

func TestProgressReader_HappyPathEOFAndDeadlineCleared(t *testing.T) {
	res := &stallResult{}
	srv := httptest.NewServer(drainHandler(1<<20, 300*time.Millisecond, res))
	defer srv.Close()

	// Two sequential requests on a keep-alive connection: the second must
	// not inherit a stale read deadline from the first (Close clears it).
	for i := 0; i < 2; i++ {
		resp, err := srv.Client().Post(srv.URL, "application/octet-stream", io.LimitReader(neverEnding('y'), 32<<10))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if n, rerr := res.get(); rerr != nil || n != 32<<10 {
			t.Fatalf("request %d: n=%d err=%v", i, n, rerr)
		}
	}
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
