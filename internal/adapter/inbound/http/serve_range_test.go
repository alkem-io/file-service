package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/adapter/outbound/storage/local"
	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
)

// countingStorage counts ReadStream calls while returning the real adapter's
// handle untouched, so the served blob stays the seekable *os.File production uses.
type countingStorage struct {
	port.StoragePort
	readStreams int
}

func (s *countingStorage) ReadStream(externalID string) (io.ReadCloser, int64, error) {
	s.readStreams++
	return s.StoragePort.ReadStream(externalID)
}

type rangeFixture struct {
	handler *PublicHandler
	storage *countingStorage
	docID   uuid.UUID
	etag    string
	payload []byte
}

// newRangeFixture stores a distinguishable 1000-byte video blob in the real
// local adapter and serves it through a PublicHandler with auth allowed.
func newRangeFixture(t *testing.T) rangeFixture {
	t.Helper()
	payload := make([]byte, 1000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	adapter := local.New(t.TempDir())
	stored, err := adapter.Save(payload)
	if err != nil {
		t.Fatalf("save blob: %v", err)
	}
	storage := &countingStorage{StoragePort: adapter}
	doc := model.Document{
		ID: uuid.New(), ExternalID: stored.ExternalID, MimeType: "video/mp4", AuthorizationID: uuid.New(),
	}
	return rangeFixture{
		handler: &PublicHandler{
			Repo:    &mockDocRepo{doc: doc},
			Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
			Storage: storage,
			MaxAge:  86400,
			Logger:  zap.NewNop(),
		},
		storage: storage,
		docID:   doc.ID,
		etag:    `"` + stored.ExternalID + `"`,
		payload: payload,
	}
}

func (f rangeFixture) serve(header map[string]string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", f.handler.ServeDocument)
	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+f.docID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// The serve contract a cache and the media element rely on, on both 200 and 206.
func assertServeHeaders(t *testing.T, rr *httptest.ResponseRecorder, etag string) {
	t.Helper()
	for name, want := range map[string]string{
		"Content-Type":           "video/mp4",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    "inline",
		"Cache-Control":          "public, max-age=86400",
		"ETag":                   etag,
		"Accept-Ranges":          "bytes",
	} {
		if got := rr.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// Safari's media stack opens every <video> with a Range request and will not
// play a resource whose server answers it with the whole body.
func TestServeDocument_RangeRequestsAnswerPartialContent(t *testing.T) {
	for _, tc := range []struct {
		rangeHeader  string
		start, end   int // inclusive
		contentRange string
	}{
		{"bytes=0-1", 0, 1, "bytes 0-1/1000"},
		{"bytes=100-199", 100, 199, "bytes 100-199/1000"},
		{"bytes=990-", 990, 999, "bytes 990-999/1000"},
		{"bytes=-10", 990, 999, "bytes 990-999/1000"},
	} {
		f := newRangeFixture(t)
		rr := f.serve(map[string]string{"Range": tc.rangeHeader})

		if rr.Code != http.StatusPartialContent {
			t.Fatalf("%s: status = %d, want 206", tc.rangeHeader, rr.Code)
		}
		if got := rr.Header().Get("Content-Range"); got != tc.contentRange {
			t.Errorf("%s: Content-Range = %q, want %q", tc.rangeHeader, got, tc.contentRange)
		}
		want := f.payload[tc.start : tc.end+1]
		if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(len(want)) {
			t.Errorf("%s: Content-Length = %q, want %d", tc.rangeHeader, got, len(want))
		}
		if !bytes.Equal(rr.Body.Bytes(), want) {
			t.Errorf("%s: body is not bytes %d-%d of the blob", tc.rangeHeader, tc.start, tc.end)
		}
		assertServeHeaders(t, rr, f.etag)
	}
}

func TestServeDocument_NoRangeServesWholeBlob(t *testing.T) {
	f := newRangeFixture(t)
	rr := f.serve(nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Length"); got != "1000" {
		t.Errorf("Content-Length = %q, want 1000", got)
	}
	if !bytes.Equal(rr.Body.Bytes(), f.payload) {
		t.Error("body is not the whole blob")
	}
	assertServeHeaders(t, rr, f.etag)
}

func TestServeDocument_UnsatisfiableRange(t *testing.T) {
	f := newRangeFixture(t)
	rr := f.serve(map[string]string{"Range": "bytes=1000-"})

	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rr.Code)
	}
	if got := rr.Header().Get("Content-Range"); got != "bytes */1000" {
		t.Errorf("Content-Range = %q, want bytes */1000", got)
	}
	if bytes.Contains(rr.Body.Bytes(), f.payload[:16]) {
		t.Error("416 body leaked blob bytes")
	}
}

// If-Range lets a player resume only while the blob is unchanged; a stale
// validator must get the whole current blob, never a slice of a different one.
func TestServeDocument_IfRange(t *testing.T) {
	f := newRangeFixture(t)
	rr := f.serve(map[string]string{"Range": "bytes=0-1", "If-Range": f.etag})
	if rr.Code != http.StatusPartialContent {
		t.Errorf("matching If-Range: status = %d, want 206", rr.Code)
	}

	rr = f.serve(map[string]string{"Range": "bytes=0-1", "If-Range": `"stale"`})
	if rr.Code != http.StatusOK {
		t.Fatalf("stale If-Range: status = %d, want 200", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), f.payload) {
		t.Error("stale If-Range: body is not the whole blob")
	}
}

func TestServeDocument_DeniedRangeNeverOpensStorage(t *testing.T) {
	f := newRangeFixture(t)
	f.handler.Auth = &mockAuth{result: model.AuthResult{Allowed: false}}
	rr := f.serve(map[string]string{"Range": "bytes=0-1"})

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if f.storage.readStreams != 0 {
		t.Errorf("ReadStream called %d times for a denied request, want 0", f.storage.readStreams)
	}
}
