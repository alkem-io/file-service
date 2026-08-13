package http

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// ptr is a small generic helper for the optional-value document fields.
func ptr[T any](v T) *T { return &v }

// --- by-reference lookup (GET /internal/file/by-reference) ---

func TestByReference_GlobalResolvesAcrossBuckets(t *testing.T) {
	h, repo, _ := newDocHandler()
	w, ht := 320, 240
	repo.refDoc = &model.Document{
		ID:                uuid.New(),
		ExternalID:        "hashabc",
		MimeType:          "image/png",
		Size:              99,
		DisplayName:       "m.png",
		StorageBucketID:   uuid.New(),
		AuthorizationID:   uuid.New(),
		ExternalReference: ptr("media_id_1"),
		ImageWidth:        &w,
		ImageHeight:       &ht,
	}

	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=media_id_1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.refCalls != 1 || repo.lastRefKey != "media_id_1" {
		t.Errorf("GetByReference not called with the ref: calls=%d key=%q", repo.refCalls, repo.lastRefKey)
	}
	if repo.refInBucketCalls != 0 {
		t.Errorf("bucketId omitted must resolve GLOBALLY: scoped variant called %d times", repo.refInBucketCalls)
	}
	body := rr.Body.String()
	for _, want := range []string{`"externalReference":"media_id_1"`, `"imageWidth":320`, `"imageHeight":240`, `"externalID":"hashabc"`} {
		if !strings.Contains(body, want) {
			t.Errorf("by-reference response missing %s; body=%s", want, body)
		}
	}
}

// A bucketId in the query must dispatch to the bucket-SCOPED lookup, carrying
// that bucket — never to the global one. The two repo variants are scripted
// with DIFFERENT documents and separate counters, so the assertion can actually
// fail if the dispatch is inverted.
func TestByReference_ScopedUsesBucketVariant(t *testing.T) {
	h, repo, _ := newDocHandler()
	bucketID := uuid.New()
	// Scripted so that resolving GLOBALLY yields a *different* document (and
	// resolving in the wrong direction yields a 404 instead of a 200).
	repo.refDoc = &model.Document{ID: uuid.New(), ExternalID: "global-hash", MimeType: "text/plain", ExternalReference: ptr("media_id_2")}
	repo.refInBucketDoc = &model.Document{ID: uuid.New(), ExternalID: "scoped-hash", MimeType: "text/plain", ExternalReference: ptr("media_id_2")}

	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=media_id_2&bucketId="+bucketID.String(), nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if repo.refInBucketCalls != 1 || repo.refCalls != 0 {
		t.Errorf("bucketId present must resolve SCOPED: scoped=%d global=%d", repo.refInBucketCalls, repo.refCalls)
	}
	if repo.lastRefBucket != bucketID {
		t.Errorf("scoped lookup used bucket %v, want %v", repo.lastRefBucket, bucketID)
	}
	if !strings.Contains(rr.Body.String(), `"externalID":"scoped-hash"`) {
		t.Errorf("response did not come from the scoped lookup; body=%s", rr.Body.String())
	}
}

func TestByReference_MissingRefIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing ref", rr.Code)
	}
}

func TestByReference_InvalidBucketIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=x&bucketId=not-a-uuid", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid bucketId", rr.Code)
	}
}

// Only an ABSENT bucketId selects the GLOBAL (cross-bucket) lookup. A parameter
// that is present but empty — `&bucketId=`, what an interpolated-but-unset
// caller variable produces — must be a 400, never a silent widening of a
// bucket-scoped read into a cross-bucket one.
func TestByReference_PresentButEmptyBucketIs400(t *testing.T) {
	for _, query := range []string{"?ref=x&bucketId=", "?ref=x&bucketId"} {
		h, repo, _ := newDocHandler()
		// Scripted so a widened GLOBAL resolution would SUCCEED — the test can
		// only pass because the handler refuses, not because nothing matched.
		repo.refDoc = &model.Document{ID: uuid.New(), ExternalID: "global-hash", MimeType: "text/plain"}

		r := chi.NewRouter()
		r.Get("/internal/file/by-reference", h.ByReference)
		req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference"+query, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 for a present-but-empty bucketId, body: %s", query, rr.Code, rr.Body.String())
		}
		if repo.refCalls != 0 {
			t.Errorf("%s: an empty bucketId widened the read to a GLOBAL lookup (refCalls=%d)", query, repo.refCalls)
		}
	}
}

func TestByReference_NotFoundIs404(t *testing.T) {
	h, repo, _ := newDocHandler()
	repo.refDoc = nil // → ErrDocumentNotFound
	r := chi.NewRouter()
	r.Get("/internal/file/by-reference", h.ByReference)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/by-reference?ref=missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// --- create: externalReference, optional authorizationId, skip ordering ---

// buildCreateBody writes an ordered multipart body: the metadata fields in the
// given order, followed by the file part LAST when fileLast=true, else the file
// FIRST. Ordering matters only for skipImageProcessing (consumed when the file
// is staged).
func buildCreateBody(t *testing.T, fields [][2]string, fileLast bool, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	wr := multipart.NewWriter(&body)
	writeFile := func() {
		part, _ := wr.CreateFormFile("file", "blob")
		_, _ = part.Write(content)
	}
	if !fileLast {
		writeFile()
	}
	for _, f := range fields {
		_ = wr.WriteField(f[0], f[1])
	}
	if fileLast {
		writeFile()
	}
	_ = wr.Close()
	return &body, wr.FormDataContentType()
}

func TestCreate_PersistsExternalReference(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		{"externalReference", "media_id_abc"},
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ExternalReference == nil || *repo.lastCreateDoc.ExternalReference != "media_id_abc" {
		t.Errorf("externalReference not persisted: %v", repo.lastCreateDoc.ExternalReference)
	}
}

// The Synapse media provider stores staging docs with NO authorizationId; the
// server mints one on inbound re-home. Create must accept an omitted
// authorizationId (201) and store the zero UUID → NULL.
func TestCreate_OmittedAuthorizationId_StoresNullAuth(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"externalReference", "media_id_blob"},
		{"skipImageProcessing", "true"},
		// authorizationId intentionally omitted
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.AuthorizationID != uuid.Nil {
		t.Errorf("AuthorizationID = %v, want uuid.Nil (NULL) when omitted", repo.lastCreateDoc.AuthorizationID)
	}
}

// skipImageProcessing=true established AFTER the file part is rejected — the
// bytes may already have been transcoded, so the byte-exact contract cannot be
// honored.
func TestCreate_SkipImageProcessingAfterFileIs400(t *testing.T) {
	h, _, _ := newDocHandler()
	// file FIRST, skipImageProcessing after.
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		{"skipImageProcessing", "true"},
	}, false, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (skipImageProcessing after file), body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "skipImageProcessing must be sent before the file part") {
		t.Errorf("body = %q, want the ordering error", rr.Body.String())
	}
}

// --- PATCH move + re-attribute ---

func TestPatch_ReattributeAuthorizationId(t *testing.T) {
	docID := uuid.New()
	newAuth := uuid.New()
	rr, repo := runPatch(t, docID, `{"authorizationId":"`+newAuth.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 1 {
		t.Fatalf("UpdateMetadata calls = %d, want 1", repo.updateMetadataCalls)
	}
	if repo.lastUpdateMeta.AuthorizationID == nil || *repo.lastUpdateMeta.AuthorizationID != newAuth {
		t.Errorf("re-attributed authorizationId = %v, want %v", repo.lastUpdateMeta.AuthorizationID, newAuth)
	}
}

func TestPatch_AuthorizationIdCannotBeCleared(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"authorizationId":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (auth cannot be cleared), body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("rejected PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

func TestPatch_ExternalReference_SetAndClear(t *testing.T) {
	docID := uuid.New()

	// set
	rr, repo := runPatch(t, docID, `{"externalReference":"media_id_new"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("set: status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateMeta.ExternalReference == nil || *repo.lastUpdateMeta.ExternalReference != "media_id_new" {
		t.Errorf("externalReference set = %v, want media_id_new", repo.lastUpdateMeta.ExternalReference)
	}

	// clear via explicit null
	rr2, repo2 := runPatch(t, docID, `{"externalReference":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), ExternalReference: ptr("media_id_old"), Version: 1}
	})
	if rr2.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, body: %s", rr2.Code, rr2.Body.String())
	}
	if repo2.updateMetadataCalls != 1 || repo2.lastUpdateMeta.ExternalReference != nil {
		t.Errorf("externalReference not cleared: calls=%d ref=%v", repo2.updateMetadataCalls, repo2.lastUpdateMeta.ExternalReference)
	}
}

func TestPatch_ExternalReference_EmptyStringRejected(t *testing.T) {
	docID := uuid.New()
	rr, _ := runPatch(t, docID, `{"externalReference":""}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty externalReference, body: %s", rr.Code, rr.Body.String())
	}
}

// A case-variant JSON key must still resolve to the canonical field and apply
// its re-attribute — otherwise a half-applied, security-relevant re-home could
// silently drop the change.
func TestPatch_CaseInsensitiveKey_AppliesReattribute(t *testing.T) {
	docID := uuid.New()
	newAuth := uuid.New()
	rr, repo := runPatch(t, docID, `{"AuthorizationId":"`+newAuth.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateMeta.AuthorizationID == nil || *repo.lastUpdateMeta.AuthorizationID != newAuth {
		t.Errorf("case-variant key dropped the re-attribute: %v", repo.lastUpdateMeta.AuthorizationID)
	}
}

// An explicit-null on an already-null field is a no-op → idempotent 200, no write.
func TestPatch_ExplicitNullNoOp_Is200NoWrite(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"createdBy":null}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1} // CreatedBy already nil
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 idempotent no-op, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("no-op PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

// A blank (whitespace-only) externalReference is no more a usable lookup key
// than an empty one — content-dedup filters IS NULL and by-reference rejects an
// empty ref — so create must normalize it to NULL rather than persist it.
func TestCreate_BlankExternalReference_StoresNull(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		{"externalReference", "   "},
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ExternalReference != nil {
		t.Errorf("externalReference = %q, want nil (NULL) for a whitespace-only value", *repo.lastCreateDoc.ExternalReference)
	}
}

// authorizationId is optional by ABSENCE ONLY. A part that IS present must
// carry a real policy id: blank/whitespace is malformed input (develop's 400),
// and the literal zero UUID is the adapter's SQL-NULL sentinel, so accepting
// either would silently store a policy-less row for a caller that believes it
// supplied a policy.
func TestCreate_PresentButInvalidAuthorizationIdIs400(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"zeroUUID", uuid.Nil.String()},
		{"malformed", "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			body, ct := buildCreateBody(t, [][2]string{
				{"displayName", "m.bin"},
				{"storageBucketId", uuid.New().String()},
				{"authorizationId", tc.value},
			}, true, []byte("hello"))

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a present-but-invalid authorizationId, body: %s", rr.Code, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("rejected create must not have written a row")
			}
		})
	}
}

// The verbatim contract (013): skipImageProcessing must reach StageUpload
// through the multipart wiring, not merely be parsed. For a TRANSCODABLE image
// type the two arms are externally distinguishable — the encoder runs, or the
// bytes pass through and the dims are measured from the completed stage — so
// this pins the flag's POSITIVE effect at the HTTP boundary rather than trusting
// a service-layer call that bypasses the handler.
func TestCreate_SkipImageProcessing_TakesVerbatimArm(t *testing.T) {
	for _, tc := range []struct {
		skip          string
		wantTranscode int
		wantMeasure   int
	}{
		{"true", 0, 1},
		{"false", 1, 0},
	} {
		t.Run("skip="+tc.skip, func(t *testing.T) {
			h, _, storage, processor := newDocHandlerWithProcessor()
			processor.detectMIME = "image/png" // transcodable → the arms diverge
			content := []byte("\x89PNG\r\n\x1a\npretend-png-bytes")

			body, ct := buildCreateBody(t, [][2]string{
				{"skipImageProcessing", tc.skip}, // must precede the file part
				{"displayName", "m.png"},
				{"storageBucketId", uuid.New().String()},
				{"authorizationId", uuid.New().String()},
			}, true, content)

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
			}
			if processor.transcodeCalls != tc.wantTranscode {
				t.Errorf("TranscodeStream calls = %d, want %d", processor.transcodeCalls, tc.wantTranscode)
			}
			if processor.measureDimsCalls != tc.wantMeasure {
				t.Errorf("MeasureDims calls = %d, want %d", processor.measureDimsCalls, tc.wantMeasure)
			}
			if !bytes.Equal(storage.saved, content) {
				t.Errorf("stored bytes = %q, want the upload verbatim", storage.saved)
			}
		})
	}
}

// A metadata part whose name isn't recognized is a 400, not a silent drop. An
// exact-match collector that ignores what it doesn't know turns a caller typo
// into a 201 that stored the WRONG thing: "skipimageprocessing" mis-cased means
// the verbatim contract was silently transcoded, "externalRef" means a bridge
// document with no reference that the provider can never resolve.
func TestCreate_UnknownMultipartFieldIs400(t *testing.T) {
	for _, field := range []string{
		"skipimageprocessing", // mis-cased near-miss on the verbatim flag
		"SkipImageProcessing",
		"externalRef", // misspelled near-miss on the bridge key
		"external_reference",
		"storagebucketid",
		"totallyUnknown",
	} {
		t.Run(field, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			body, ct := buildCreateBody(t, [][2]string{
				{"displayName", "m.bin"},
				{"storageBucketId", uuid.New().String()},
				{"authorizationId", uuid.New().String()},
				{field, "true"},
			}, true, []byte("hello"))

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for the unknown field %q, body: %s", rr.Code, field, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("a request with an unknown field must not have written a row")
			}
		})
	}
}

// The verbatim decision is taken when the file part is staged and is
// irreversible — the bytes are already written. A later skipImageProcessing part
// that CONTRADICTS it must be rejected in BOTH directions, so the request record
// can never claim a processed store for content that was stored verbatim (or the
// reverse). The consistent restatement stays a 201.
func TestCreate_ContradictingSkipImageProcessingPartIs400(t *testing.T) {
	for _, tc := range []struct {
		name       string
		before     string
		after      string
		wantStatus int
	}{
		{"false after verbatim stage", "true", "false", http.StatusBadRequest},
		{"true after processed stage", "false", "true", http.StatusBadRequest},
		{"consistent restatement (true)", "true", "true", http.StatusCreated},
		{"consistent restatement (false)", "false", "false", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _, processor := newDocHandlerWithProcessor()
			processor.detectMIME = "image/png" // transcodable → the two arms actually diverge

			// skipImageProcessing FIRST (so it governs staging), file, then the
			// duplicate part.
			var body bytes.Buffer
			wr := multipart.NewWriter(&body)
			_ = wr.WriteField("skipImageProcessing", tc.before)
			part, _ := wr.CreateFormFile("file", "blob")
			_, _ = part.Write([]byte("\x89PNG\r\n\x1a\npretend"))
			_ = wr.WriteField("displayName", "m.png")
			_ = wr.WriteField("storageBucketId", uuid.New().String())
			_ = wr.WriteField("authorizationId", uuid.New().String())
			_ = wr.WriteField("skipImageProcessing", tc.after)
			_ = wr.Close()

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", &body)
			req.Header.Set("Content-Type", wr.FormDataContentType())
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus != http.StatusCreated {
				if repo.lastCreateDoc.ID != uuid.Nil {
					t.Error("a contradicted skipImageProcessing must not have written a row")
				}
				return
			}
			// On the accepted path the stored bytes and the staging decision must
			// agree: skip=true means the transcoder never ran.
			wantTranscode := 1
			if tc.before == "true" {
				wantTranscode = 0
			}
			if processor.transcodeCalls != wantTranscode {
				t.Errorf("TranscodeStream calls = %d, want %d", processor.transcodeCalls, wantTranscode)
			}
		})
	}
}

// A reference Postgres cannot store as text — invalid UTF-8, or a NUL — must be
// a clean 400 BEFORE anything is published. Without the up-front check the
// INSERT fails server-side and surfaces as a 500 that has already committed a
// blob nothing will ever reference (blob cleanup is deliberately never done on
// the create error path).
func TestCreate_UnstorableExternalReferenceIs400WithoutPublishing(t *testing.T) {
	for _, tc := range []struct{ name, ref string }{
		{"invalid utf-8", "media_\xff\xfe_id"},
		{"embedded NUL", "media\x00id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, storage := newDocHandler()
			body, ct := buildCreateBody(t, [][2]string{
				{"displayName", "m.bin"},
				{"storageBucketId", uuid.New().String()},
				{"authorizationId", uuid.New().String()},
				{"externalReference", tc.ref},
			}, true, []byte("hello"))

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("rejected create must not have written a row")
			}
			if len(storage.stages) != 1 || storage.stages[0].committed {
				t.Errorf("rejected create published a blob (orphan): stages=%d", len(storage.stages))
			}
		})
	}
}

// The same rule on the JSON paths, where a NUL arrives as a \u0000 escape.
func TestCopyAndPatch_UnstorableExternalReferenceIs400(t *testing.T) {
	sourceID := uuid.New()

	h, repo, _ := newDocHandler()
	repo.doc = model.Document{ID: sourceID, ExternalID: "hash", MimeType: "image/png", DisplayName: "b.png"}
	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)
	copyBody := `{"sourceId":"` + sourceID.String() +
		`","destinationBucketId":"` + uuid.New().String() +
		`","authorizationId":"` + uuid.New().String() +
		`","externalReference":"media\u0000id"}`
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(copyBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("copy: status = %d, want 400 for a NUL-bearing externalReference, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ID != uuid.Nil {
		t.Error("copy: rejected request must not have written a row")
	}

	docID := uuid.New()
	prr, prepo := runPatch(t, docID, `{"externalReference":"media\u0000id"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if prr.Code != http.StatusBadRequest {
		t.Errorf("patch: status = %d, want 400, body: %s", prr.Code, prr.Body.String())
	}
	if prepo.updateMetadataCalls != 0 {
		t.Errorf("patch: rejected request must not write (calls=%d)", prepo.updateMetadataCalls)
	}
}

// file."displayName" is character data too, so the SAME unstorable bytes are the
// same orphan-blob 500 — and the control-character rule cannot substitute for the
// encoding check: ranging over a string DECODES an invalid byte to U+FFFD
// (> 0x20), so an invalid-UTF-8 name passes every other displayName rule. 013's
// inbound re-home names the file, and the multipart create path carries arbitrary
// bytes, so this is reachable. (Invalid UTF-8 cannot arrive on the JSON paths —
// encoding/json substitutes U+FFFD while decoding — so the create path is where
// it must be caught; a NUL is rejected on both.)
func TestCreate_UnstorableDisplayNameIs400WithoutPublishing(t *testing.T) {
	h, repo, storage := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "photo_\xff\xfe.png"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an invalid-UTF-8 displayName, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ID != uuid.Nil {
		t.Error("rejected create must not have written a row")
	}
	if len(storage.stages) != 1 || storage.stages[0].committed {
		t.Errorf("rejected create published a blob (orphan): stages=%d", len(storage.stages))
	}
}

// The nil-UUID rule is uniform across ALL THREE paths, not just PATCH: an
// optional owner/tagset that IS supplied must be a real id. uuidToPgxNullable
// writes a LITERAL all-zero UUID (NOT NULL) for it, which then reads back out
// through meta / by-reference as an owner "00000000-…" that matches no actor —
// the very sentinel leak this branch closed for authorizationId. Omitting the
// field remains the way to say "no owner" (asserted separately), so this adds no
// required-ness.
func TestCreateAndCopy_ZeroUUIDOptionalIdsAre400(t *testing.T) {
	for _, field := range []string{"createdBy", "tagsetId"} {
		t.Run("create/"+field, func(t *testing.T) {
			h, repo, storage := newDocHandler()
			body, ct := buildCreateBody(t, [][2]string{
				{"displayName", "m.bin"},
				{"storageBucketId", uuid.New().String()},
				{"authorizationId", uuid.New().String()},
				{field, uuid.Nil.String()},
			}, true, []byte("hello"))

			r := chi.NewRouter()
			r.Post("/internal/file", h.Create)
			req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
			req.Header.Set("Content-Type", ct)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a nil-UUID %s, body: %s", rr.Code, field, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("rejected create must not have written a row")
			}
			if len(storage.stages) != 1 || storage.stages[0].committed {
				t.Errorf("rejected create published a blob (orphan): stages=%d", len(storage.stages))
			}
		})

		t.Run("copy/"+field, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			sourceID := uuid.New()
			repo.doc = model.Document{ID: sourceID, ExternalID: "hash", MimeType: "image/png", DisplayName: "b.png"}

			body := `{"sourceId":"` + sourceID.String() +
				`","destinationBucketId":"` + uuid.New().String() +
				`","authorizationId":"` + uuid.New().String() +
				`","` + field + `":"` + uuid.Nil.String() + `"}`

			r := chi.NewRouter()
			r.Post("/internal/file/copy", h.Copy)
			req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a nil-UUID %s, body: %s", rr.Code, field, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("rejected copy must not have written a row")
			}
		})
	}
}

// The counterpart the rejection above must not swallow: OMITTING createdBy /
// tagsetId is still how a caller says "no owner / no tagset" (the Synapse media
// provider sends neither), and must stay a 201 that stores NULL.
func TestCreate_OmittedOptionalIdsStoreNull(t *testing.T) {
	h, repo, _ := newDocHandler()
	body, ct := buildCreateBody(t, [][2]string{
		{"displayName", "m.bin"},
		{"storageBucketId", uuid.New().String()},
		{"authorizationId", uuid.New().String()},
		// createdBy / tagsetId intentionally omitted
	}, true, []byte("hello"))

	r := chi.NewRouter()
	r.Post("/internal/file", h.Create)
	req := httptest.NewRequest(http.MethodPost, "/internal/file", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.CreatedBy != nil {
		t.Errorf("createdBy = %v, want nil (NULL) when omitted", repo.lastCreateDoc.CreatedBy)
	}
	if repo.lastCreateDoc.TagsetID != nil {
		t.Errorf("tagsetId = %v, want nil (NULL) when omitted", repo.lastCreateDoc.TagsetID)
	}
}

// The all-zero UUID is the adapter's SQL-NULL sentinel, so it is not a VALUE on
// any re-attribute field — clearing is expressed with an explicit JSON null and
// nothing else. createdBy is clearable and still must reject it: persisting the
// zero UUID would report a real owner that matches no actor.
func TestPatch_ZeroUUIDRejectedOnBothReattributeFields(t *testing.T) {
	for _, field := range []string{"authorizationId", "createdBy"} {
		docID := uuid.New()
		creator := uuid.New()
		rr, repo := runPatch(t, docID, `{"`+field+`":"`+uuid.Nil.String()+`"}`, func(repo *mockDocRepo) {
			repo.doc = model.Document{
				ID: docID, StorageBucketID: uuid.New(),
				AuthorizationID: uuid.New(), CreatedBy: &creator, Version: 1,
			}
		})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 for the nil UUID, body: %s", field, rr.Code, rr.Body.String())
		}
		if repo.updateMetadataCalls != 0 {
			t.Errorf("%s: rejected PATCH must not write (calls=%d)", field, repo.updateMetadataCalls)
		}
	}
}

// UpdateDocumentMetadata overwrites the authorizationId / createdBy columns
// unconditionally, so a PATCH that does NOT mention them must RE-SUPPLY their
// current values. Nothing else defends this: dropping the seeds would silently
// NULL a re-homed conversation attachment's policy and owner on the very
// request that moves it — unreadable (403 on every serve) and unattributable,
// with no error anywhere.
func TestPatch_OmittedReattributeFieldsAreResupplied(t *testing.T) {
	docID := uuid.New()
	currentAuth := uuid.New()
	currentCreator := uuid.New()
	destBucket := uuid.New()

	// The real 013 re-home: MOVE the staged document into the conversation
	// bucket, naming ONLY storageBucketId.
	rr, repo := runPatch(t, docID, `{"storageBucketId":"`+destBucket.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{
			ID:                docID,
			StorageBucketID:   uuid.New(),
			DisplayName:       "attachment.png",
			AuthorizationID:   currentAuth,
			CreatedBy:         &currentCreator,
			ExternalReference: ptr("media_id_rehome"),
			Version:           1,
		}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 1 {
		t.Fatalf("UpdateMetadata calls = %d, want 1", repo.updateMetadataCalls)
	}
	meta := repo.lastUpdateMeta
	if meta.StorageBucketID != destBucket {
		t.Errorf("storageBucketId = %v, want the move target %v", meta.StorageBucketID, destBucket)
	}
	if meta.AuthorizationID == nil || *meta.AuthorizationID != currentAuth {
		t.Errorf("omitted authorizationId = %v, want the row's current %v re-supplied (an unconditional "+
			"UPDATE would NULL the policy and orphan the document)", meta.AuthorizationID, currentAuth)
	}
	if meta.CreatedBy == nil || *meta.CreatedBy != currentCreator {
		t.Errorf("omitted createdBy = %v, want the row's current %v re-supplied", meta.CreatedBy, currentCreator)
	}
	if meta.ExternalReference == nil || *meta.ExternalReference != "media_id_rehome" {
		t.Errorf("omitted externalReference = %v, want media_id_rehome re-supplied", meta.ExternalReference)
	}
	if meta.DisplayName != "attachment.png" {
		t.Errorf("omitted displayName = %q, want the row's current value re-supplied", meta.DisplayName)
	}
}

// The mirror case: a re-attribute that names ONE field must not drop the other.
func TestPatch_ReattributeOneFieldPreservesTheOther(t *testing.T) {
	docID := uuid.New()
	currentCreator := uuid.New()
	newAuth := uuid.New()

	rr, repo := runPatch(t, docID, `{"authorizationId":"`+newAuth.String()+`"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{
			ID: docID, StorageBucketID: uuid.New(), AuthorizationID: uuid.New(),
			CreatedBy: &currentCreator, Version: 1,
		}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastUpdateMeta.AuthorizationID == nil || *repo.lastUpdateMeta.AuthorizationID != newAuth {
		t.Errorf("authorizationId = %v, want the re-attributed %v", repo.lastUpdateMeta.AuthorizationID, newAuth)
	}
	if repo.lastUpdateMeta.CreatedBy == nil || *repo.lastUpdateMeta.CreatedBy != currentCreator {
		t.Errorf("createdBy = %v, want the untouched current %v", repo.lastUpdateMeta.CreatedBy, currentCreator)
	}
}

// --- copy: authorizationId is mandatory and must be a real policy ---

// The zero UUID parses cleanly but is the adapter's SQL-NULL sentinel, so a copy
// that supplies it must be a 400 — never a silently policy-less row materialized
// through an endpoint with no staging semantics.
func TestCopy_ZeroAuthorizationIdIs400(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{ID: sourceID, ExternalID: "hash", MimeType: "image/png"}

	body := `{"sourceId":"` + sourceID.String() +
		`","destinationBucketId":"` + uuid.New().String() +
		`","authorizationId":"` + uuid.Nil.String() + `"}`

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for the nil-UUID authorizationId, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ID != uuid.Nil {
		t.Error("rejected copy must not have written a row")
	}
}

// Copy normalizes a blank externalReference to NULL, exactly like create.
func TestCopy_BlankExternalReference_StoresNull(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{ID: sourceID, ExternalID: "hash", MimeType: "image/png", DisplayName: "b.png"}

	body := `{"sourceId":"` + sourceID.String() +
		`","destinationBucketId":"` + uuid.New().String() +
		`","authorizationId":"` + uuid.New().String() +
		`","externalReference":"  "}`

	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.ExternalReference != nil {
		t.Errorf("externalReference = %q, want nil (NULL) for a whitespace-only value", *repo.lastCreateDoc.ExternalReference)
	}
}

// --- PATCH: "supplies no updatable field" is a 400, not an idempotent 200 ---

// The no-op 200 exists so a PATCH naming REAL values that already match the row
// doesn't spuriously 409 a concurrent re-home. It is not a licence to accept a
// body that supplies nothing: an explicit null on a field that has no "clear"
// semantics (storageBucketId, temporaryLocation, displayName) instructs nothing
// at all, so it stays the 400 it was before 013.
func TestPatch_NullOnlyOnNonClearableFieldsIs400(t *testing.T) {
	for _, body := range []string{
		`{"displayName":null}`,
		`{"storageBucketId":null}`,
		`{"temporaryLocation":null}`,
		`{"storageBucketId":null,"temporaryLocation":null,"displayName":null}`,
	} {
		docID := uuid.New()
		rr, repo := runPatch(t, docID, body, func(repo *mockDocRepo) {
			repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), DisplayName: "keep.txt", Version: 1}
		})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400, body: %s", body, rr.Code, rr.Body.String())
		}
		if repo.updateMetadataCalls != 0 {
			t.Errorf("%s: rejected PATCH must not write (calls=%d)", body, repo.updateMetadataCalls)
		}
	}
}

// The counterpart the 400 above must not swallow: a PATCH that NAMES a real
// value which happens to equal the current one is an idempotent 200 that writes
// nothing (so a concurrent re-home is never spuriously 409'd).
func TestPatch_MatchingValueIs200NoWrite(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"displayName":"keep.txt"}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), DisplayName: "keep.txt", Version: 1}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 idempotent no-op, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("no-op PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

// PATCH is tri-state, so it CLEARS with an explicit null; a blank string value
// is neither "keep" nor "clear" and is rejected — the same blank-is-not-a-
// reference rule create/copy apply by normalizing to NULL.
func TestPatch_BlankExternalReferenceRejected(t *testing.T) {
	docID := uuid.New()
	rr, repo := runPatch(t, docID, `{"externalReference":"   "}`, func(repo *mockDocRepo) {
		repo.doc = model.Document{ID: docID, StorageBucketID: uuid.New(), Version: 1}
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a whitespace-only externalReference, body: %s", rr.Code, rr.Body.String())
	}
	if repo.updateMetadataCalls != 0 {
		t.Errorf("rejected PATCH must not write (calls=%d)", repo.updateMetadataCalls)
	}
}

// --- responses never render a NULL authorizationId as the zero UUID ---

// A staging document has no authorization (NULL column, read back as the zero
// UUID). Rendering "00000000-0000-0000-0000-000000000000" would report it as
// AUTHORIZED under an all-zero policy, so every response carrying the field
// omits it instead — and still emits it for a real policy.
func TestResponses_OmitAuthorizationIdWhenAbsent(t *testing.T) {
	authID := uuid.New()
	docID := uuid.New()

	staging := model.Document{
		ID: docID, ExternalID: "h", MimeType: "image/png", StorageBucketID: uuid.New(),
		AuthorizationID: uuid.Nil, ExternalReference: ptr("media_id_staged"),
	}
	authorized := staging
	authorized.AuthorizationID = authID

	routes := []struct {
		name  string
		verb  string
		path  string
		mount func(chi.Router, *DocumentHandler)
	}{
		{"meta", http.MethodGet, "/internal/file/" + docID.String() + "/meta", func(r chi.Router, h *DocumentHandler) {
			r.Get("/internal/file/{id}/meta", h.GetMeta)
		}},
		{"by-reference", http.MethodGet, "/internal/file/by-reference?ref=media_id_staged", func(r chi.Router, h *DocumentHandler) {
			r.Get("/internal/file/by-reference", h.ByReference)
		}},
		{"delete", http.MethodDelete, "/internal/file/" + docID.String(), func(r chi.Router, h *DocumentHandler) {
			r.Delete("/internal/file/{id}", h.Delete)
		}},
	}

	for _, rt := range routes {
		serve := func(doc model.Document) map[string]any {
			t.Helper()
			h, repo, _ := newDocHandler()
			repo.doc = doc
			repo.refDoc = &doc
			repo.deleteResult = model.DeletedDocument{ExternalID: doc.ExternalID, AuthorizationID: doc.AuthorizationID}
			repo.count = 1
			r := chi.NewRouter()
			rt.mount(r, h)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(rt.verb, rt.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, body: %s", rt.name, rr.Code, rr.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s: %v", rt.name, err)
			}
			return out
		}

		if body := serve(staging); body["authorizationId"] != nil {
			t.Errorf("%s: authorizationId = %v, want the key omitted for a document with no authorization",
				rt.name, body["authorizationId"])
		}
		if body := serve(authorized); body["authorizationId"] != authID.String() {
			t.Errorf("%s: authorizationId = %v, want %v", rt.name, body["authorizationId"], authID)
		}
	}
}

// --- meta response carries externalReference + dims ---

func TestGetMeta_IncludesExternalReferenceAndDims(t *testing.T) {
	h, repo, _ := newDocHandler()
	w, ht := 128, 96
	docID := uuid.New()
	repo.doc = model.Document{
		ID: docID, ExternalID: "h", MimeType: "image/png", StorageBucketID: uuid.New(),
		AuthorizationID: uuid.New(), ExternalReference: ptr("media_id_meta"), ImageWidth: &w, ImageHeight: &ht,
	}
	r := chi.NewRouter()
	r.Get("/internal/file/{id}/meta", h.GetMeta)

	req := httptest.NewRequest(http.MethodGet, "/internal/file/"+docID.String()+"/meta", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`"externalReference":"media_id_meta"`, `"imageWidth":128`, `"imageHeight":96`} {
		if !strings.Contains(body, want) {
			t.Errorf("meta response missing %s; body=%s", want, body)
		}
	}
}
