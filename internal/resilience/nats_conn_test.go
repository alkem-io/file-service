package resilience

import (
	"testing"
	"time"
)

func TestCalculateReconnectDelay_ExponentialBackoff(t *testing.T) {
	base := 100 * time.Millisecond
	maxDelay := 10 * time.Second

	d1 := calculateReconnectDelay(base, maxDelay, 1)
	d2 := calculateReconnectDelay(base, maxDelay, 2)
	d3 := calculateReconnectDelay(base, maxDelay, 3)

	// Each attempt should roughly double (plus jitter)
	if d2 < d1 {
		t.Errorf("delay should increase: attempt2=%v < attempt1=%v", d2, d1)
	}
	if d3 < d2 {
		t.Errorf("delay should increase: attempt3=%v < attempt2=%v", d3, d2)
	}
}

func TestCalculateReconnectDelay_CapsAtMax(t *testing.T) {
	base := 100 * time.Millisecond
	maxDelay := 500 * time.Millisecond

	// attempt 10 would be 100ms * 2^9 = 51.2s without cap
	d := calculateReconnectDelay(base, maxDelay, 10)

	// Should be capped at max + jitter (jitter is max/4 = 125ms)
	upperBound := maxDelay + maxDelay/4
	if d > upperBound {
		t.Errorf("delay %v exceeds upper bound %v", d, upperBound)
	}
}

func TestCalculateReconnectDelay_FirstAttemptIsBase(t *testing.T) {
	base := 1 * time.Second
	maxDelay := 30 * time.Second

	d := calculateReconnectDelay(base, maxDelay, 1)

	// attempt 1: base * 2^0 = base, plus jitter up to base/4
	upperBound := base + base/4
	if d > upperBound {
		t.Errorf("first attempt delay %v exceeds base+jitter %v", d, upperBound)
	}
	if d < base {
		t.Errorf("first attempt delay %v less than base %v", d, base)
	}
}

func TestCalculateReconnectDelay_AlwaysPositive(t *testing.T) {
	base := 1 * time.Millisecond
	maxDelay := 1 * time.Second

	for i := 0; i < 100; i++ {
		d := calculateReconnectDelay(base, maxDelay, i)
		if d <= 0 {
			t.Fatalf("delay at attempt %d is non-positive: %v", i, d)
		}
	}
}
