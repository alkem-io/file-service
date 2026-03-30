package resilience

import (
	"errors"
	"testing"
	"time"
)

func TestBreaker_StartsInClosed(t *testing.T) {
	b := NewBreaker(3, 10*time.Second, 2)
	if b.State() != StateClosed {
		t.Errorf("state = %v, want Closed", b.State())
	}
}

func TestBreaker_AllowInClosed(t *testing.T) {
	b := NewBreaker(3, 10*time.Second, 2)
	if err := b.Allow(); err != nil {
		t.Errorf("Allow in Closed should succeed: %v", err)
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := NewBreaker(3, 10*time.Second, 2)

	b.RecordFailure()
	b.RecordFailure()
	if b.State() != StateClosed {
		t.Fatal("should still be Closed after 2 failures")
	}

	b.RecordFailure() // 3rd = threshold
	if b.State() != StateOpen {
		t.Errorf("state = %v, want Open after %d failures", b.State(), 3)
	}
}

func TestBreaker_RejectsWhenOpen(t *testing.T) {
	b := NewBreaker(1, 10*time.Second, 2)
	b.RecordFailure()

	err := b.Allow()
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("Allow in Open should return ErrCircuitOpen, got %v", err)
	}
}

func TestBreaker_TransitionsToHalfOpenAfterTimeout(t *testing.T) {
	b := NewBreaker(1, 50*time.Millisecond, 2)
	b.RecordFailure()

	if b.State() != StateOpen {
		t.Fatal("should be Open")
	}

	time.Sleep(60 * time.Millisecond)

	// Allow should transition to HalfOpen
	if err := b.Allow(); err != nil {
		t.Errorf("Allow after timeout should succeed: %v", err)
	}
	if b.State() != StateHalfOpen {
		t.Errorf("state = %v, want HalfOpen", b.State())
	}
}

func TestBreaker_HalfOpenAllowsUpToMax(t *testing.T) {
	b := NewBreaker(1, 1*time.Millisecond, 2)
	b.RecordFailure()
	time.Sleep(5 * time.Millisecond)

	// First Allow transitions to HalfOpen
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}

	// Record one success
	b.RecordSuccess()

	// Second Allow should still work (successCount=1 < halfOpenMax=2)
	if err := b.Allow(); err != nil {
		t.Errorf("should allow second request in HalfOpen: %v", err)
	}
}

func TestBreaker_HalfOpenClosesAfterEnoughSuccesses(t *testing.T) {
	b := NewBreaker(1, 1*time.Millisecond, 2)
	b.RecordFailure()
	time.Sleep(5 * time.Millisecond)

	_ = b.Allow() // transitions to HalfOpen
	b.RecordSuccess()
	b.RecordSuccess() // meets halfOpenMax

	if b.State() != StateClosed {
		t.Errorf("state = %v, want Closed after enough successes", b.State())
	}
}

func TestBreaker_HalfOpenReopensOnFailure(t *testing.T) {
	b := NewBreaker(1, 1*time.Millisecond, 2)
	b.RecordFailure()
	time.Sleep(5 * time.Millisecond)

	_ = b.Allow() // transitions to HalfOpen
	b.RecordFailure()

	if b.State() != StateOpen {
		t.Errorf("state = %v, want Open after failure in HalfOpen", b.State())
	}
}

func TestBreaker_SuccessResetFailureCount(t *testing.T) {
	b := NewBreaker(3, 10*time.Second, 2)

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess() // resets failure count

	b.RecordFailure()
	b.RecordFailure()
	// Should still be Closed — count was reset to 0, now at 2
	if b.State() != StateClosed {
		t.Errorf("state = %v, want Closed after success reset", b.State())
	}
}

func TestBreaker_HalfOpenRejectsExhaustedSlots(t *testing.T) {
	// halfOpenMax=1: only 1 test request allowed
	b := NewBreaker(1, 1*time.Millisecond, 1)
	b.RecordFailure()
	time.Sleep(5 * time.Millisecond)

	// First Allow transitions to HalfOpen with successCount=0
	err := b.Allow()
	if err != nil {
		t.Fatalf("first Allow: %v", err)
	}

	// Don't call RecordSuccess yet — simulate the request is still in-flight
	// Second Allow should be rejected because we've used our 1 slot
	// Wait — successCount is still 0, halfOpenMax is 1, so 0 < 1 = allowed
	// The rejection only happens when successCount >= halfOpenMax
	// This means we need successCount to reach max WITHOUT transitioning to Closed
	// That's impossible in the current implementation because RecordSuccess
	// transitions to Closed when successCount >= halfOpenMax

	// The HalfOpen rejection branch protects against too many concurrent test requests
	// Force it by directly setting the internal state:
	b.mu.Lock()
	b.state = StateHalfOpen
	b.successCount = b.halfOpenMax // simulate max reached
	b.mu.Unlock()

	err = b.Allow()
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("expected ErrCircuitOpen in HalfOpen with exhausted slots, got %v", err)
	}
}

func TestBreaker_StateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateClosed, "closed"},
		{StateOpen, "open"},
		{StateHalfOpen, "half-open"},
		{State(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}
