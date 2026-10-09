package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/alkem-io/file-service/internal/domain/model"
)

func TestConditionalDeleteContract(t *testing.T) {
	bucket := uuid.New().String()
	for _, tc := range []struct {
		name, query string
		repoErr     error
		status      int
	}{
		{"ordinary", "", nil, 200},
		{"ordinary missing", "", model.ErrDocumentNotFound, 404},
		{"conditional", "?expectedStorageBucketId=" + bucket, nil, 200},
		{"conditional moved or missing", "?expectedStorageBucketId=" + bucket, model.ErrDocumentNotFound, 409},
		{"conditional backend failure", "?expectedStorageBucketId=" + bucket, errors.New("unavailable"), 500},
		{"empty condition", "?expectedStorageBucketId=", nil, 400},
		{"invalid condition", "?expectedStorageBucketId=invalid", nil, 400},
		{"nil condition", "?expectedStorageBucketId=" + uuid.Nil.String(), nil, 400},
		{"multiple conditions", "?expectedStorageBucketId=" + bucket + "&expectedStorageBucketId=" + bucket, nil, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, repo, _ := newDocHandler()
			repo.deleteErr = tc.repoErr
			repo.count = 1 // no physical cleanup in this HTTP mapping test
			router := chi.NewRouter()
			router.Delete("/internal/file/{id}", handler.Delete)
			result := httptest.NewRecorder()
			router.ServeHTTP(result, httptest.NewRequest(http.MethodDelete, "/internal/file/"+uuid.New().String()+tc.query, nil))
			if result.Code != tc.status {
				t.Fatalf("HTTP %d, expected %d: %s", result.Code, tc.status, result.Body.String())
			}
			if repo.getByIDCalls != 0 {
				t.Fatal("DELETE must not introduce a read then unconditional delete")
			}
		})
	}
}
