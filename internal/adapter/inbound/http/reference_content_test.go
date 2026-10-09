package http

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/service"
)

func referenceRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set(HeaderActorID, "actor-1")
	return r
}

func referenceFixture(t *testing.T) (*mockDocRepo, *mockAuth, *countingStorage, http.Handler, string) {
	t.Helper()
	f := newRangeFixture(t)
	doc := f.handler.Repo.(*mockDocRepo).doc
	doc.StorageBucketID = uuid.New()
	ref := "opaque +/reference"
	doc.ExternalReference = &ref
	width, height := 640, 480
	doc.ImageWidth, doc.ImageHeight = &width, &height
	repo := &mockDocRepo{doc: doc, refDoc: &doc, refInBucketDoc: &doc}
	auth := f.handler.Auth.(*mockAuth)
	f.handler.Repo = repo
	h := &DocumentHandler{Service: &service.FileService{Repo: repo, Storage: f.storage}, Logger: zap.NewNop()}
	router := NewRouter(Deps{PublicHandler: f.handler, DocumentHandler: h, Logger: zap.NewNop()})
	return repo, auth, f.storage, router, url.QueryEscape(ref)
}

func TestReferenceContentRoutes(t *testing.T) {
	for _, internal := range []bool{false, true} {
		name := "public"
		if internal {
			name = "internal"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				method, rangeValue string
				status, length     int
				contentRange       string
			}{
				{"GET", "", 200, 1000, ""}, {"HEAD", "", 200, 0, ""}, {"GET", "bytes=0-99", 206, 100, "bytes 0-99/1000"}, {"GET", "bytes=9999-", 416, -1, "bytes */1000"},
			} {
				t.Run(tc.method+tc.rangeValue, func(t *testing.T) {
					repo, _, _, router, ref := referenceFixture(t)
					path := "/rest/storage/file/by-reference?bucketId=" + repo.doc.StorageBucketID.String() + "&ref=" + ref
					if internal {
						path = "/internal/file/by-reference/content?ref=" + ref
					}
					rr := httptest.NewRecorder()
					req := referenceRequest(tc.method, path)
					req.Header.Set("Range", tc.rangeValue)
					router.ServeHTTP(rr, req)
					if rr.Code != tc.status {
						t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
					}
					if tc.length >= 0 && rr.Body.Len() != tc.length {
						t.Fatalf("length %d", rr.Body.Len())
					}
					assertReferenceMetadata(t, rr, tc.method, tc.status)
					if rr.Header().Get("Content-Range") != tc.contentRange {
						t.Fatal("wrong range")
					}
					if repo.getByIDCalls != 0 {
						t.Fatal("reference read must not lookup file ID")
					}
					if internal && repo.refInBucketCalls != 0 || !internal && repo.refCalls != 0 {
						t.Fatal("wrong lookup scope")
					}
					if rr.Header().Get("Location") != "" {
						t.Fatal("must not redirect")
					}
				})
			}
		})
	}
}

func TestPublicReferenceAuthorizationPrecedesConditionalAndRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		policy  bool
		authErr error
		status  int
	}{
		{"denied", false, true, nil, 403}, {"staging", true, false, nil, 403}, {"auth unavailable", false, true, errors.New("offline"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, auth, storage, router, ref := referenceFixture(t)
			auth.result.Allowed = tc.allowed
			auth.err = tc.authErr
			if !tc.policy {
				repo.refInBucketDoc.AuthorizationID = uuid.Nil
			}
			req := referenceRequest("GET", "/rest/storage/file/by-reference?bucketId="+repo.doc.StorageBucketID.String()+"&ref="+ref)
			req.Header.Set("Range", "bytes=0-1")
			req.Header.Set("If-None-Match", `"`+repo.doc.ExternalID+`"`)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.status || storage.readStreams != 0 {
				t.Fatalf("status=%d reads=%d", rr.Code, storage.readStreams)
			}
		})
	}
}

func TestReferenceContentValidationAndNoCrossBucketFallback(t *testing.T) {
	for _, q := range []string{"", "?ref=x", "?bucketId=&ref=x", "?bucketId=" + uuid.Nil.String() + "&ref=x", "?bucketId=" + uuid.NewString() + "&ref=", "?bucketId=" + uuid.NewString() + "&ref=" + strings.Repeat("x", 257), "?bucketId=%zz&ref=x", "?bucketId=" + uuid.NewString() + "&ref=x&ref=y"} {
		repo, _, storage, router, _ := referenceFixture(t)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, referenceRequest("GET", "/rest/storage/file/by-reference"+q))
		if rr.Code != 400 || storage.readStreams != 0 || repo.refCalls != 0 || repo.refInBucketCalls != 0 {
			t.Fatalf("query %q status %d", q, rr.Code)
		}
	}
	repo, _, storage, router, ref := referenceFixture(t)
	repo.refInBucketDoc = nil
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, referenceRequest("GET", "/rest/storage/file/by-reference?bucketId="+uuid.NewString()+"&ref="+ref))
	if rr.Code != 404 || storage.readStreams != 0 || repo.refCalls != 0 {
		t.Fatalf("missing association leaked global source: %d", rr.Code)
	}
}

func TestReferenceGETAndHEADUseSameStoredBytes(t *testing.T) {
	_, _, _, router, ref := referenceFixture(t)
	server := httptest.NewServer(router)
	defer server.Close()
	uri := server.URL + "/internal/file/by-reference/content?ref=" + ref
	getRequest, _ := http.NewRequest("GET", uri, nil)
	get, err := server.Client().Do(getRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = get.Body.Close() }()
	body, _ := io.ReadAll(get.Body)
	expected := make([]byte, 1000)
	for i := range expected {
		expected[i] = byte(i % 251)
	}
	if !bytes.Equal(body, expected) {
		t.Fatal("bytes changed")
	}
	req, _ := http.NewRequest("HEAD", uri, nil)
	head, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = head.Body.Close() }()
	b, _ := io.ReadAll(head.Body)
	if len(b) != 0 || head.ContentLength != 1000 {
		t.Fatal("invalid HEAD")
	}
}

type vanishedBlob struct{ *countingStorage }

func (s *vanishedBlob) ReadStream(key string) (io.ReadCloser, int64, error) {
	if err := s.Delete(key); err != nil {
		return nil, 0, err
	}
	return s.countingStorage.ReadStream(key)
}

func TestReferenceReadDeletionRaceReturnsUnavailable(t *testing.T) {
	repo, _, storage, _, ref := referenceFixture(t)
	handler := &DocumentHandler{Service: &service.FileService{Repo: repo, Storage: &vanishedBlob{storage}}, Logger: zap.NewNop()}
	rr := httptest.NewRecorder()
	handler.ContentByReference(rr, referenceRequest("GET", "/internal/file/by-reference/content?ref="+ref))
	if rr.Code != 404 || repo.refCalls != 1 || repo.getByIDCalls != 0 {
		t.Fatalf("status=%d global=%d id=%d", rr.Code, repo.refCalls, repo.getByIDCalls)
	}
}

func TestReferenceNotModifiedAndAnonymousBoundary(t *testing.T) {
	repo, auth, storage, router, ref := referenceFixture(t)
	path := "/rest/storage/file/by-reference?bucketId=" + repo.doc.StorageBucketID.String() + "&ref=" + ref
	req := referenceRequest("GET", path)
	req.Header.Set("If-None-Match", `"`+repo.doc.ExternalID+`"`)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 304 || storage.readStreams != 0 || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("304 contract %d", rr.Code)
	}
	if auth.lastPrivilege != "read" {
		t.Fatal("304 skipped authorization")
	}
	req = referenceRequest("GET", path)
	req.Header.Del(HeaderActorID)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 401 || storage.readStreams != 0 {
		t.Fatal("gateway identity boundary bypassed")
	}
}

func TestInternalReferenceMissingAndMalformed(t *testing.T) {
	for _, query := range []string{"", "?ref=", "?ref=%00", "?ref=" + strings.Repeat("x", 257), "?ref=%zz", "?ref=a&ref=b"} {
		repo, _, storage, router, _ := referenceFixture(t)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, referenceRequest("GET", "/internal/file/by-reference/content"+query))
		if rr.Code != 400 || repo.refCalls != 0 || storage.readStreams != 0 {
			t.Fatalf("invalid reference status=%d", rr.Code)
		}
	}
	repo, _, storage, router, ref := referenceFixture(t)
	repo.refDoc = nil
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, referenceRequest("GET", "/internal/file/by-reference/content?ref="+ref))
	if rr.Code != 404 || storage.readStreams != 0 {
		t.Fatal("missing reference did not return unavailable")
	}
}

func assertReferenceMetadata(t *testing.T, rr *httptest.ResponseRecorder, method string, status int) {
	t.Helper()
	if status < 400 {
		if rr.Header().Get("Content-Type") != "video/mp4" || rr.Header().Get("X-Alkemio-Image-Width") != "640" || rr.Header().Get("X-Alkemio-Image-Height") != "480" {
			t.Fatalf("stored metadata missing: %v", rr.Header())
		}
		if method == "HEAD" && rr.Header().Get("Content-Length") != "1000" {
			t.Fatal("HEAD length missing")
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing hardening")
		}
	}
}
