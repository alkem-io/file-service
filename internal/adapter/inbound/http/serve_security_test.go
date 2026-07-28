package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// newServeHandler wires a PublicHandler serving a single document of the given
// MIME type, with auth allowed and a fixed blob body.
func newServeHandler(mimeType string) (*PublicHandler, uuid.UUID) {
	docID := uuid.New()
	return &PublicHandler{
		Repo: &mockDocRepo{doc: model.Document{
			ID: docID, ExternalID: "abc", MimeType: mimeType, AuthorizationID: uuid.New(),
		}},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{data: []byte("payload")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}, docID
}

// Stored-XSS hardening (013): every served response carries nosniff, and the
// enumerable set of browser-active/script-capable content types is forced to
// download via Content-Disposition: attachment while inert media serves inline.
func TestServeDocument_SecurityHeaders(t *testing.T) {
	for _, tc := range []struct {
		mime            string
		wantDisposition string
	}{
		{"text/html", "attachment"},
		{"application/xhtml+xml", "attachment"},
		{"image/svg+xml", "attachment"},
		{"application/xml", "attachment"},
		{"application/xslt+xml", "attachment"},
		{"text/xsl", "attachment"},
		{"message/rfc822", "attachment"},
		{"image/png", "inline"},
		{"application/pdf", "inline"},
		{"text/plain", "inline"},
		{"application/octet-stream", "inline"},
	} {
		h, docID := newServeHandler(tc.mime)
		r := chi.NewRouter()
		r.Get("/rest/storage/document/{id}", h.ServeDocument)
		req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.mime, rr.Code)
		}
		if ns := rr.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", tc.mime, ns)
		}
		if cd := rr.Header().Get("Content-Disposition"); cd != tc.wantDisposition {
			t.Errorf("%s: Content-Disposition = %q, want %q", tc.mime, cd, tc.wantDisposition)
		}
	}
}
