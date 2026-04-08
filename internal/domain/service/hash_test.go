package service

import "testing"

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
