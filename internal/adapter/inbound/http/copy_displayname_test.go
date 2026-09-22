package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

// copyBody builds a POST /internal/file/copy payload, including displayName
// only when one is supplied, so the omission case exercises a genuinely absent
// JSON key rather than an empty string.
func copyBody(t *testing.T, sourceID uuid.UUID, displayName *string) []byte {
	t.Helper()
	payload := map[string]any{
		"sourceId":            sourceID.String(),
		"destinationBucketId": uuid.New().String(),
		"authorizationId":     uuid.New().String(),
	}
	if displayName != nil {
		payload["displayName"] = *displayName
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func postCopy(t *testing.T, h *DocumentHandler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/internal/file/copy", h.Copy)
	req := httptest.NewRequest(http.MethodPost, "/internal/file/copy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// A SUPPLIED displayName goes through the same validateDisplayName contract as
// every other stored name. Rejection happens at the boundary, before any row is
// written — a bad name must not leave a half-materialized copy behind.
func TestCopy_InvalidSuppliedDisplayNameIs400(t *testing.T) {
	cases := map[string]string{
		"path separator":    "dir/evil.png",
		"backslash":         "dir\\evil.png",
		"control character": "bad\x01name.png",
		"whitespace only":   "   ",
		"too long":          strings.Repeat("a", 513),
	}

	for name, displayName := range cases {
		t.Run(name, func(t *testing.T) {
			h, repo, _ := newDocHandler()
			sourceID := uuid.New()
			repo.doc = model.Document{ID: sourceID, ExternalID: "hash", MimeType: "image/png"}

			rr := postCopy(t, h, copyBody(t, sourceID, &displayName))

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %s, body: %s", rr.Code, name, rr.Body.String())
			}
			if repo.lastCreateDoc.ID != uuid.Nil {
				t.Error("rejected copy must not have written a row")
			}
		})
	}
}

// A VALID supplied displayName reaches the new row. The Synapse storage
// provider names its row after the media id, so a conversation copy must be
// able to carry the event's real filename instead.
func TestCopy_ValidSuppliedDisplayNameReachesTheRow(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:          sourceID,
		ExternalID:  "hash",
		MimeType:    "image/png",
		DisplayName: "KnJLupUceCirVxKYoDGsrbdC",
	}

	supplied := "holiday.png"
	rr := postCopy(t, h, copyBody(t, sourceID, &supplied))

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.DisplayName != supplied {
		t.Errorf("stored DisplayName = %q, want the supplied %q",
			repo.lastCreateDoc.DisplayName, supplied)
	}
}

// Omitting displayName inherits the source's name, so existing callers that
// never send the field keep their present behaviour.
func TestCopy_OmittedDisplayNameInheritsSource(t *testing.T) {
	h, repo, _ := newDocHandler()
	sourceID := uuid.New()
	repo.doc = model.Document{
		ID:          sourceID,
		ExternalID:  "hash",
		MimeType:    "image/png",
		DisplayName: "banner.png",
	}

	rr := postCopy(t, h, copyBody(t, sourceID, nil))

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body: %s", rr.Code, rr.Body.String())
	}
	if repo.lastCreateDoc.DisplayName != "banner.png" {
		t.Errorf("stored DisplayName = %q, want the inherited %q",
			repo.lastCreateDoc.DisplayName, "banner.png")
	}
}
