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

// detectorXMLMIMEs are the `+xml` media types THIS SERVICE'S OWN MIME detector
// can emit — every `+xml` string in gabriel-vasile/mimetype v1.4.13 (the vips
// build's DetectMIME), which is what lands in file."mimeType" on ingest.
//
// The point of enumerating them here is that an enumeration is the wrong shape
// for the PRODUCTION deny-list: only four of these were ever named there, so ten
// content types the service stores by itself served INLINE. A browser routes
// every one of them through its XML parser, where an <?xml-stylesheet?>
// processing instruction loads XSLT and executes script in this origin — the
// stored-XSS the hardening exists to close. The handler therefore matches the
// `+xml` FAMILY, and this list is the regression fence: adding a detector
// version with new dialects cannot silently reopen the hole.
var detectorXMLMIMEs = []string{
	"application/atom+xml",
	"application/gml+xml",
	"application/gpx+xml",
	"application/owl+xml",
	"application/rss+xml",
	"application/vnd.garmin.tcx+xml",
	"application/vnd.google-earth.kml+xml",
	"application/vnd.ms-package.3dmanufacturing-3dmodel+xml",
	"application/vnd.ms-visio.drawing.main+xml",
	"application/xhtml+xml",
	"application/x-xliff+xml",
	"image/svg+xml",
	"model/vnd.collada+xml",
	"model/x3d+xml",
}

// Every `+xml` type the detector can produce must be forced to download. A
// caller may also DECLARE any other `+xml` type (an empty upload keeps the
// declared type verbatim), so the family match — not a list — is the invariant.
func TestServeDocument_EveryXMLFamilyTypeIsActiveContent(t *testing.T) {
	cases := append([]string{}, detectorXMLMIMEs...)
	cases = append(cases,
		// Family roots, which carry no +xml suffix (RFC 7303).
		"application/xml", "text/xml",
		// Dialects no detector here emits but a caller can declare.
		"application/rdf+xml", "application/mathml+xml", "application/xslt+xml",
		"application/vnd.mozilla.xul+xml", "application/dash+xml",
		// Normalization-dependent spellings.
		"Application/GPX+XML", "application/gpx+xml; charset=utf-8", "  model/x3d+xml  ",
	)

	for _, mime := range cases {
		h, docID := newServeHandlerFor(model.Document{
			ExternalID: "abc", MimeType: mime, DisplayName: "payload.xml", AuthorizationID: uuid.New(),
		})
		rr := serveOne(h, docID)

		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", mime, rr.Code)
		}
		if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
			t.Errorf("%s: Content-Disposition = %q, want an attachment — every XML-family type "+
				"renders as XML and can execute script via an XSLT processing instruction", mime, cd)
		}
	}
}

// The family match must not swallow inert types that merely mention xml, or the
// in-browser preview parity UX regresses into forced downloads.
func TestServeDocument_XMLLookalikesStayInline(t *testing.T) {
	for _, mime := range []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/xml-dtd",
		"text/xml-external-parsed-entity",
		"image/png",
		"application/pdf",
	} {
		h, docID := newServeHandlerFor(model.Document{
			ExternalID: "abc", MimeType: mime, AuthorizationID: uuid.New(),
		})
		rr := serveOne(h, docID)
		if cd := rr.Header().Get("Content-Disposition"); cd != "inline" {
			t.Errorf("%s: Content-Disposition = %q, want inline", mime, cd)
		}
	}
}

// A conditional request that revalidates must still carry the hardening. A 304
// has no body, but a cache UPDATES its stored response's headers from it (RFC
// 9110 §15.4.5) — so a 304 that omitted nosniff / Content-Disposition would let
// a cached copy predating the hardening stay unhardened and keep being served.
func TestServeDocument_NotModifiedCarriesHardening(t *testing.T) {
	for _, tc := range []struct{ mime, wantDisposition string }{
		{"text/html", `attachment; filename="page.html"; filename*=UTF-8''page.html`},
		{"image/png", "inline"},
	} {
		h, docID := newServeHandlerFor(model.Document{
			ExternalID: "abc", MimeType: tc.mime, DisplayName: "page.html", AuthorizationID: uuid.New(),
		})

		r := chi.NewRouter()
		r.Get("/rest/storage/document/{id}", h.ServeDocument)
		req := httptest.NewRequest(http.MethodGet, "/rest/storage/document/"+docID.String(), nil)
		req.Header.Set("If-None-Match", `"abc"`)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyActorID, "actor-1"))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusNotModified {
			t.Fatalf("%s: status = %d, want 304", tc.mime, rr.Code)
		}
		if ns := rr.Header().Get("X-Content-Type-Options"); ns != "nosniff" {
			t.Errorf("%s: 304 X-Content-Type-Options = %q, want nosniff", tc.mime, ns)
		}
		if cd := rr.Header().Get("Content-Disposition"); cd != tc.wantDisposition {
			t.Errorf("%s: 304 Content-Disposition = %q, want %q", tc.mime, cd, tc.wantDisposition)
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
