package resilience

import (
	"errors"
	"sync"
	"time"
)

// State represents the circuit breaker state.
type State int

const (
	StateClosed   State = iota // Normal operation
	StateOpen                  // Rejecting requests
	StateHalfOpen              // Testing recovery
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

var ErrCircuitOpen = errors.New("circuit breaker is open")

// Breaker implements a simple circuit breaker pattern.
type Breaker struct {
	mu               sync.Mutex
	state            State
	failureCount     int
	successCount     int
	failureThreshold int
	openTimeout      time.Duration
	halfOpenMax      int
	lastFailure      time.Time
}

// NewBreaker creates a circuit breaker with the given settings.
func NewBreaker(failureThreshold int, openTimeout time.Duration, halfOpenMax int) *Breaker {
	return &Breaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		openTimeout:      openTimeout,
		halfOpenMax:      halfOpenMax,
	}
}

// Allow checks if a request should be allowed through.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(b.lastFailure) > b.openTimeout {
			b.state = StateHalfOpen
			b.successCount = 0
			return nil
		}
		return ErrCircuitOpen
	case StateHalfOpen:
		if b.successCount >= b.halfOpenMax {
			return ErrCircuitOpen
		}
		return nil
	default:
		return nil
	}
}

// RecordSuccess records a successful operation.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateHalfOpen:
		b.successCount++
		if b.successCount >= b.halfOpenMax {
			b.state = StateClosed
			b.failureCount = 0
		}
	case StateClosed:
		b.failureCount = 0
	}
}

// RecordFailure records a failed operation.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lastFailure = time.Now()

	switch b.state {
	case StateClosed:
		b.failureCount++
		if b.failureCount >= b.failureThreshold {
			b.state = StateOpen
		}
	case StateHalfOpen:
		b.state = StateOpen
	}
}

// State returns the current breaker state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
