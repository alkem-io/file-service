// Package local implements port.StoragePort on the local filesystem with
// content-addressed blobs: a file's name is the hash of its bytes, so
// identical content dedups to one blob. Uploads stream through a staging
// temp file that is hashed while written and published by rename on Commit
// (spec 020); external IDs are allow-list validated to keep every path
// inside the configured base directory.
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alkem-io/file-service/internal/domain/model"
	"github.com/alkem-io/file-service/internal/domain/port"
	"github.com/alkem-io/file-service/internal/domain/service"
)

// Adapter implements port.StoragePort using the local filesystem.
type Adapter struct {
	basePath string
}

func New(basePath string) *Adapter {
	return &Adapter{basePath: basePath}
}

// Save stores a complete buffer. Implemented on top of the streaming stage
// (spec 020) so there is exactly one write/publish code path per backend.
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

func (a *Adapter) Read(externalID string) ([]byte, error) {
	if !isValidExternalID(externalID) {
		return nil, fmt.Errorf("invalid external ID: %s", externalID)
	}
	data, err := os.ReadFile(a.filePath(externalID))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", externalID, err)
	}
	return data, nil
}

func (a *Adapter) Delete(externalID string) error {
	if !isValidExternalID(externalID) {
		return fmt.Errorf("invalid external ID: %s", externalID)
	}
	err := os.Remove(a.filePath(externalID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", externalID, err)
	}
	return nil
}

func (a *Adapter) Exists(externalID string) (bool, error) {
	if !isValidExternalID(externalID) {
		return false, fmt.Errorf("invalid external ID: %s", externalID)
	}
	_, err := os.Stat(a.filePath(externalID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (a *Adapter) filePath(externalID string) string {
	return filepath.Join(a.basePath, externalID)
}

// isValidExternalID rejects path-traversal attempts and other dangerous
// characters in storage IDs. Accepts both formats produced across the
// service's history:
//   - SHA3-256 hex: 64 lowercase hex chars (current Go file-service format)
//   - IPFS CIDv0:   46 base58btc chars starting with "Qm" (legacy TS file-service)
//
// Conservative allow-list: alphanumeric only, length 32-128. This blocks
// '/', '\', '.', '..', NUL, control chars, whitespace — anything that
// could escape the basePath.
func isValidExternalID(id string) bool {
	if len(id) < 32 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// stage is the filesystem StageWriter: a temp file in the blob directory,
// hashed while written, published by rename on Commit (spec 020).
type stage struct {
	a         *Adapter
	tmp       *os.File
	tmpName   string
	hasher    *service.Hasher
	size      int
	committed bool
	aborted   bool
}

// OpenStage implements port.StoragePort (spec 020).
func (a *Adapter) OpenStage(_ context.Context) (port.StageWriter, error) {
	if err := os.MkdirAll(a.basePath, 0o750); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	tmp, err := os.CreateTemp(a.basePath, ".stage-*")
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
	finalPath := s.a.filePath(externalID)

	// Content-addressable dedup: blob already published → discard staging.
	if _, err := os.Stat(finalPath); err == nil {
		if rmErr := os.Remove(s.tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
			// The blob is safe (finalPath exists) but the staging artifact
			// leaked — surface it rather than accumulate orphans.
			return model.StoredFile{}, fmt.Errorf("remove staging file on dedup: %w", rmErr)
		}
		s.committed = true
		return model.StoredFile{ExternalID: externalID, Size: s.size, Created: false}, nil
	}

	if err := os.Rename(s.tmpName, finalPath); err != nil {
		return model.StoredFile{}, fmt.Errorf("publish staged file: %w", err)
	}
	s.committed = true
	return model.StoredFile{ExternalID: externalID, Size: s.size, Created: true}, nil
}

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
