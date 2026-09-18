package service

import (
	"context"
	"crypto/sha3"
	"encoding/hex"
	"hash"
	"io"
)

// Hasher computes the content identity (hex SHA3-256) incrementally — the
// streaming counterpart of ComputeHash, digest-identical by construction.
type Hasher struct {
	h hash.Hash
}

// NewHasher returns a streaming content-identity hasher (spec 020 FR-002).
func NewHasher() *Hasher {
	return &Hasher{h: sha3.New256()}
}

func (s *Hasher) Write(p []byte) (int, error) { return s.h.Write(p) }

// Sum returns the hex-encoded digest of everything written so far.
func (s *Hasher) Sum() string {
	return hex.EncodeToString(s.h.Sum(nil))
}

// ComputeHash computes the SHA3-256 hash of content and returns the hex-encoded string.
// Output is byte-identical to Node.js: createHash('sha3-256').update(data).digest('hex')
func ComputeHash(content []byte) string {
	h := NewHasher()
	_, _ = h.Write(content)
	return h.Sum()
}

// HashReadCloser streams a content hash while honoring cancellation. Closing
// the reader on cancellation interrupts a blocked filesystem read; the context
// reader also stops between successful reads. The function owns and closes the
// supplied reader on every path.
func HashReadCloser(ctx context.Context, reader io.ReadCloser) (string, error) {
	hasher := NewHasher()
	copyDone := make(chan struct{})
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		select {
		case <-ctx.Done():
			_ = reader.Close()
		case <-copyDone:
		}
	}()

	_, copyErr := io.Copy(hasher, contextReader{ctx: ctx, reader: reader})
	close(copyDone)
	<-closeDone
	closeErr := reader.Close()

	if err := ctx.Err(); err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hasher.Sum(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err == nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
	}
	return n, err
}
