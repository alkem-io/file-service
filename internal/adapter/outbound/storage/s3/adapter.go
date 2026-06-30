// Package s3 implements port.StoragePort against an S3-compatible object store
// (AWS S3, MinIO, Scaleway Object Storage) with content-addressed blobs: an
// object's key is the SHA3-256 hash of its bytes, so identical content dedups
// to one object. Uploads stream through a local staging temp file that is
// hashed while written; on Commit the staged bytes are published under their
// content hash only if no object already holds that key (StatObject dedup),
// so concurrent commits of identical content are idempotent (the bytes are
// equal by construction). This mirrors the local adapter's semantics — there
// is no atomic rename in object storage, so the content-addressed key itself
// is the idempotency primitive.
package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// Config holds the S3 connection settings.
type Config struct {
	Endpoint  string // host[:port], no scheme (e.g. "s3.fr-par.scw.cloud")
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
	// StageDir hosts the local hash-while-upload staging files. Empty =
	// os.TempDir(). Publish reads the staged file and FPutObjects it.
	StageDir string
	// Logger receives best-effort warnings (e.g. a leaked staging file whose
	// removal failed after a durable publish). Nil falls back to a no-op.
	Logger *zap.Logger
}

// Adapter implements port.StoragePort using an S3-compatible object store.
type Adapter struct {
	client   *minio.Client
	bucket   string
	stageDir string
	logger   *zap.Logger
}

// New constructs the S3 adapter and its underlying client. It does not create
// the bucket — provisioning is an infra concern.
func New(cfg Config) (*Adapter, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("init s3 client: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Adapter{client: client, bucket: cfg.Bucket, stageDir: cfg.StageDir, logger: logger}, nil
}

// Save stores a complete buffer on top of the streaming stage so there is one
// write/publish code path per backend.
func (a *Adapter) Save(content []byte) (model.StoredFile, error) {
	st, err := a.OpenStage(context.Background())
	if err != nil {
		return model.StoredFile{}, err
	}
	if _, err := st.Write(content); err != nil {
		_ = st.Abort()
		return model.StoredFile{}, err
	}
	stored, err := st.Commit()
	if err != nil {
		_ = st.Abort()
		return model.StoredFile{}, err
	}
	return stored, nil
}

// Read returns the whole blob. A missing object maps to os.ErrNotExist (via
// errors.Is) so callers treat it like the local backend's not-found.
func (a *Adapter) Read(externalID string) ([]byte, error) {
	obj, err := a.client.GetObject(context.Background(), a.bucket, externalID, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("read object %s: %w", externalID, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read object %s: %w", externalID, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("read object %s: %w", externalID, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read object %s: %w", externalID, err)
	}
	return data, nil
}

// Delete removes the blob. Idempotent: object stores do not error on deleting
// a missing key, so refcount-driven cleanup cannot race-fail.
func (a *Adapter) Delete(externalID string) error {
	if err := a.client.RemoveObject(context.Background(), a.bucket, externalID, minio.RemoveObjectOptions{}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete object %s: %w", externalID, err)
	}
	return nil
}

// Exists stats the object without fetching it. (false, nil) is a definitive
// "not there"; any other stat failure is returned as an error.
func (a *Adapter) Exists(externalID string) (bool, error) {
	_, err := a.client.StatObject(context.Background(), a.bucket, externalID, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat object %s: %w", externalID, err)
}

// isNotFound reports whether an S3 error means the object/key is absent.
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound
}

// stage is the S3 StageWriter: a local temp file, hashed while written, then
// published to the content-addressed object key on Commit.
type stage struct {
	a         *Adapter
	tmp       *os.File
	tmpName   string
	hasher    *service.Hasher
	size      int
	committed bool
	aborted   bool
}

// OpenStage begins a streaming ingestion into a local staging file. Nothing is
// observable in the object store until Commit publishes it.
func (a *Adapter) OpenStage(_ context.Context) (port.StageWriter, error) {
	dir := a.stageDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".s3stage-*")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	return &stage{a: a, tmp: tmp, tmpName: tmp.Name(), hasher: service.NewHasher()}, nil
}

func (s *stage) Write(p []byte) (int, error) {
	if s.committed || s.aborted {
		return 0, fmt.Errorf("stage: write after commit/abort")
	}
	n, err := s.tmp.Write(p)
	if n > 0 {
		_, _ = s.hasher.Write(p[:n])
		s.size += n
	}
	if err != nil {
		return n, fmt.Errorf("write staging file: %w", err)
	}
	return n, nil
}

// StagedReaderAt exposes the staging temp file for random-access inspection
// before Commit (e.g. reading a zip central directory). Valid only between
// writes and Commit/Abort.
func (s *stage) StagedReaderAt() (io.ReaderAt, int64, error) {
	if s.committed || s.aborted {
		return nil, 0, fmt.Errorf("stage: reader after commit/abort")
	}
	if err := s.tmp.Sync(); err != nil {
		return nil, 0, fmt.Errorf("sync staging file: %w", err)
	}
	return s.tmp, int64(s.size), nil
}

// Commit publishes the staged file under its content hash. The content-
// addressed key is the idempotency primitive: if an object with this key
// already exists (StatObject hit) the upload is a dedup (Created=false);
// otherwise the staged bytes are uploaded (Created=true). Concurrent commits
// of identical content converge — both compute the same key and the bytes are
// equal by construction. The staging file is always removed afterwards.
func (s *stage) Commit() (model.StoredFile, error) {
	if s.committed {
		return model.StoredFile{}, fmt.Errorf("stage: double commit")
	}
	if s.aborted {
		return model.StoredFile{}, fmt.Errorf("stage: commit after abort")
	}
	if err := s.tmp.Close(); err != nil {
		return model.StoredFile{}, fmt.Errorf("close staging file: %w", err)
	}

	externalID := s.hasher.Sum()
	ctx := context.Background()

	exists, err := s.a.Exists(externalID)
	if err != nil {
		return model.StoredFile{}, fmt.Errorf("dedup stat: %w", err)
	}

	created := false
	if !exists {
		if _, err := s.a.client.FPutObject(ctx, s.a.bucket, externalID, s.tmpName, minio.PutObjectOptions{
			ContentType: "application/octet-stream",
		}); err != nil {
			return model.StoredFile{}, fmt.Errorf("publish object %s: %w", externalID, err)
		}
		created = true
	}

	if rmErr := os.Remove(s.tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
		// The object is durably published — failing the request here would 500
		// a successful commit. Log the leaked staging artifact and return the
		// committed file; orphan temp files are an operational cleanup concern,
		// not a request failure.
		s.a.logger.Warn("s3 stage: failed to remove staging file after publish",
			zap.String("externalID", externalID),
			zap.String("tmpName", s.tmpName),
			zap.Error(rmErr))
	}
	s.committed = true
	return model.StoredFile{ExternalID: externalID, Size: s.size, Created: created}, nil
}

// Abort closes and removes the staging file. Safe at any point — a no-op after
// a successful Commit or a prior Abort — so callers can defer it.
func (s *stage) Abort() error {
	if s.aborted || s.committed {
		return nil
	}
	s.aborted = true
	_ = s.tmp.Close()
	if err := os.Remove(s.tmpName); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove staging file: %w", err)
	}
	return nil
}

// compile-time interface checks.
var (
	_ port.StoragePort = (*Adapter)(nil)
	_ port.StageWriter = (*stage)(nil)
)
