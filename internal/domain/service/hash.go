package service

import (
	"crypto/sha3"
	"encoding/hex"
	"hash"
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
