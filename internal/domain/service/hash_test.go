package service

import (
	"bytes"
	"testing"
)

func TestComputeHash_NodeJSCompatibility(t *testing.T) {
	// Reference values from Node.js:
	//   require('crypto').createHash('sha3-256').update(Buffer.from('hello')).digest('hex')
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "hello",
			expected: "3338be694f50c5f338814986cdf0686453a888b84f424d792af4b9202398f392",
		},
		{
			input:    "",
			expected: "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a",
		},
		{
			input:    "alkemio",
			expected: "d8ab5a05b09212fa22a5b0979bc6fd0c316390eed9c8922e5aa1250301c5bb0e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ComputeHash([]byte(tt.input))
			if got != tt.expected {
				t.Errorf("ComputeHash(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestComputeHash_Length(t *testing.T) {
	hash := ComputeHash([]byte("test"))
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(hash))
	}
}

func TestComputeHash_Deterministic(t *testing.T) {
	content := []byte("same content")
	h1 := ComputeHash(content)
	h2 := ComputeHash(content)
	if h1 != h2 {
		t.Errorf("non-deterministic: %q != %q", h1, h2)
	}
}

// Spec 020 T004 — the streaming hasher must be digest-identical to
// ComputeHash for any input and any write segmentation.
func TestNewHasher_MatchesComputeHash(t *testing.T) {
	inputs := [][]byte{
		nil,
		{},
		[]byte("a"),
		[]byte("hello world"),
		bytes.Repeat([]byte{0xA5, 0x5A, 0x00, 0xFF}, 1<<20), // 4 MiB
	}
	for i, in := range inputs {
		want := ComputeHash(in)

		// single write
		h := NewHasher()
		_, _ = h.Write(in)
		if got := h.Sum(); got != want {
			t.Errorf("input %d single write: %s != %s", i, got, want)
		}

		// arbitrary segmentation
		h2 := NewHasher()
		for off := 0; off < len(in); {
			n := 1 + (off*7+13)%4096
			if off+n > len(in) {
				n = len(in) - off
			}
			_, _ = h2.Write(in[off : off+n])
			off += n
		}
		if got := h2.Sum(); got != want {
			t.Errorf("input %d segmented writes: %s != %s", i, got, want)
		}
	}
}
