package service

import (
	"encoding/hex"

	"golang.org/x/crypto/sha3"
)

// ComputeHash computes the SHA3-256 hash of content and returns the hex-encoded string.
// Output is byte-identical to Node.js: createHash('sha3-256').update(data).digest('hex')
func ComputeHash(content []byte) string {
	h := sha3.New256()
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}
