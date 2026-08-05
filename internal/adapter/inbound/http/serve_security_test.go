package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// newServeHandler wires a PublicHandler serving a single document of the given
// MIME type, with auth allowed and a fixed blob body. displayName is left empty
// so the disposition header carries no filename unless a test sets one.
func newServeHandler(mimeType string) (*PublicHandler, uuid.UUID) {
	return newServeHandlerFor(model.Document{
		ExternalID: "abc", MimeType: mimeType, AuthorizationID: uuid.New(),
	})
}

// newServeHandlerFor wires a PublicHandler around a caller-supplied document,
// assigning it a fresh ID. Auth allows by default; the returned handler's Auth
// can be re-asserted by the caller.
func newServeHandlerFor(doc model.Document) (*PublicHandler, uuid.UUID) {
	doc.ID = uuid.New()
	return &PublicHandler{
		Repo:    &mockDocRepo{doc: doc},
		Auth:    &mockAuth{result: model.AuthResult{Allowed: true}},
		Storage: &mockStorage{data: []byte("payload")},
		MaxAge:  86400,
		Logger:  zap.NewNop(),
	}, doc.ID
}

// serveOne runs a single authenticated GET against the public serve route.
func serveOne(h *PublicHandler, docID uuid.UUID) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/rest/storage/document/{id}", h.ServeDocument)
	req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// Stored-XSS hardening (013): every served response carries nosniff, and the
// enumerable set of browser-active/script-capable content types is forced to
// download via Content-Disposition: attachment while inert media serves inline.
//
// The deny-list matches on the NORMALIZED MIME, so the parameterized
// ("; charset=…") and upper-case spellings of an active type must be denied
// too — without them a stored text/html would serve inline behind a
// `TEXT/HTML` or `text/html; charset=utf-8` column value.
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
		// Normalization-dependent spellings of the same active types.
		{"text/html; charset=utf-8", "attachment"},
		{"TEXT/HTML", "attachment"},
		{"Image/SVG+XML; charset=UTF-8", "attachment"},
		{"  text/html  ", "attachment"},
		{"image/png", "inline"},
		{"application/pdf", "inline"},
		{"text/plain", "inline"},
		{"text/plain; charset=utf-8", "inline"},
		{"application/octet-stream", "inline"},
	} {
		h, docID := newServeHandler(tc.mime)
		rr := serveOne(h, docID)

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

// A document with NO authorization policy is not publicly servable. Its
// authorizationId column is NULL, which reads back as the zero UUID; passing
// that to CheckPrivilege would delegate the decision to whatever the auth
// service does with a policy id that does not exist. file-service denies it
// itself, with the same 403 as any other denial, and never asks.
func TestServeDocument_NoAuthorization_DeniedWithoutAuthCall(t *testing.T) {
	h, docID := newServeHandlerFor(model.Document{
		ExternalID: "abc", MimeType: "image/png", AuthorizationID: uuid.Nil,
	})
	auth := &mockAuth{result: model.AuthResult{Allowed: true}} // would ALLOW if consulted
	h.Auth = auth

	rr := serveOne(h, docID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	if auth.calls != 0 {
		t.Errorf("CheckPrivilege called %d times with a non-existent policy id (%q); the deny belongs here",
			auth.calls, auth.lastAuthPolicyID)
	}
	if rr.Body.String() == "" || rr.Body.Len() > 200 {
		t.Errorf("unexpected deny body: %s", rr.Body.String())
	}
}

// A forced download must name the file, or it saves as the bare document UUID
// with no extension. displayName is caller data going into a response HEADER,
// so the name is sanitized (ASCII fallback) and RFC 5987-encoded (filename*),
// and no input can inject header content.
func TestServeDocument_AttachmentFilename(t *testing.T) {
	for _, tc := range []struct{ name, displayName, want string }{
		{
			name:        "plain ascii",
			displayName: "report.html",
			want:        `attachment; filename="report.html"; filename*=UTF-8''report.html`,
		},
		{
			name:        "space and unicode",
			displayName: "жар птица.html",
			want: `attachment; filename="______ __________.html"; ` +
				`filename*=UTF-8''%D0%B6%D0%B0%D1%80%20%D0%BF%D1%82%D0%B8%D1%86%D0%B0.html`,
		},
		{
			// A crafted name must not close the quoted string, add a parameter,
			// or start a new header line.
			name:        "header injection attempt",
			displayName: "a\";x=1\r\nSet-Cookie: p=1;.html",
			want:        `attachment; filename="a__x=1__Set-Cookie: p=1_.html"; filename*=UTF-8''a%22%3Bx%3D1%0D%0ASet-Cookie%3A%20p%3D1%3B.html`,
		},
		{
			name:        "blank name yields a bare attachment",
			displayName: "   ",
			want:        "attachment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, docID := newServeHandlerFor(model.Document{
				ExternalID: "abc", MimeType: "text/html", DisplayName: tc.displayName,
				AuthorizationID: uuid.New(),
			})
			rr := serveOne(h, docID)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			cd := rr.Header().Get("Content-Disposition")
			if cd != tc.want {
				t.Errorf("Content-Disposition = %q, want %q", cd, tc.want)
			}
			if strings.ContainsAny(cd, "\r\n") {
				t.Errorf("Content-Disposition contains a line break: %q", cd)
			}
		})
	}
}
