package s3

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/alkem-io/file-service/internal/domain/service"
)

// testAdapter constructs an S3 adapter against a live S3-compatible store.
//
// INTEGRATION INFRA REQUIRED. These tests need a reachable S3/MinIO endpoint
// and skip when it is not configured — they are NOT a fake pass. Run them with,
// e.g., a local MinIO:
//
//	docker run -p 9000:9000 minio/minio server /data
//	S3_TEST_ENDPOINT=localhost:9000 S3_TEST_ACCESS_KEY=minioadmin \
//	S3_TEST_SECRET_KEY=minioadmin S3_TEST_BUCKET=test S3_TEST_USE_SSL=false \
//	go test ./internal/adapter/outbound/storage/s3/...
//
// The bucket must already exist.
func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set — skipping S3 integration tests (needs live S3/MinIO)")
	}
	a, err := New(Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("S3_TEST_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_TEST_SECRET_KEY"),
		Bucket:    os.Getenv("S3_TEST_BUCKET"),
		Region:    os.Getenv("S3_TEST_REGION"),
		UseSSL:    os.Getenv("S3_TEST_USE_SSL") == "true",
		StageDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestS3_SaveReadDelete(t *testing.T) {
	a := testAdapter(t)
	content := []byte("hello s3 content " + t.Name())

	stored, err := a.Save(content)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if stored.ExternalID != service.ComputeHash(content) {
		t.Errorf("externalID = %q, want content hash", stored.ExternalID)
	}
	t.Cleanup(func() { _ = a.Delete(stored.ExternalID) })

	got, err := a.Read(stored.ExternalID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read-back differs from stored content")
	}

	ok, err := a.Exists(stored.ExternalID)
	if err != nil || !ok {
		t.Errorf("Exists = (%v, %v), want (true, nil)", ok, err)
	}

	if err := a.Delete(stored.ExternalID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.Read(stored.ExternalID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read after delete err = %v, want os.ErrNotExist", err)
	}
	// Delete is idempotent.
	if err := a.Delete(stored.ExternalID); err != nil {
		t.Errorf("second Delete should be a no-op, got %v", err)
	}
}

// Stage→commit dedup: committing identical content twice yields the same key
// and the second commit reports Created=false.
func TestS3_StageCommitDedup(t *testing.T) {
	a := testAdapter(t)
	content := []byte("dedup s3 payload " + t.Name())

	commit := func() (created bool, id string) {
		st, err := a.OpenStage(context.Background())
		if err != nil {
			t.Fatalf("OpenStage: %v", err)
		}
		if _, err := st.Write(content); err != nil {
			t.Fatalf("Write: %v", err)
		}
		stored, err := st.Commit()
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		return stored.Created, stored.ExternalID
	}

	created1, id1 := commit()
	t.Cleanup(func() { _ = a.Delete(id1) })
	created2, id2 := commit()

	if id1 != id2 {
		t.Errorf("dedup failed: %q != %q", id1, id2)
	}
	if !created1 {
		t.Error("first commit Created = false, want true")
	}
	if created2 {
		t.Error("second commit Created = true, want false (dedup hit)")
	}
}
