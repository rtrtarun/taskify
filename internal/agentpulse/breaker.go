package agentpulse

import (
	"sync"
	"time"
)

// BreakerState is the state of a circuit breaker.
type BreakerState string

const (
	BreakerClosed   BreakerState = "closed"
	BreakerOpen     BreakerState = "open"
	BreakerHalfOpen BreakerState = "half_open"
)

// Breaker is a circuit breaker over a rolling window of recent requests.
//
// It opens when the window is full and more than the threshold of its
// requests failed. After the cooldown it lets one trial request through:
// success closes it with an empty window, failure opens it again.
type Breaker struct {
	mu        sync.Mutex
	window    []bool // ring of outcomes; true is a failure
	next      int
	filled    int
	failures  int
	threshold float64
	cooldown  time.Duration
	now       func() time.Time

	state    BreakerState
	openedAt time.Time
	trial    bool // a half-open trial is in flight
}

// NewBreaker returns a closed breaker. The defaults in
// docs/architecture.md are a 20-request window, a 0.5 threshold, and a
// 10-second cooldown.
func NewBreaker(window int, threshold float64, cooldown time.Duration) *Breaker {
	return &Breaker{
		window:    make([]bool, window),
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		state:     BreakerClosed,
	}
}

// DefaultBreaker returns a breaker with the documented defaults.
func DefaultBreaker() *Breaker { return NewBreaker(20, 0.5, 10*time.Second) }

// WithClock replaces the time source, for tests.
func (b *Breaker) WithClock(now func() time.Time) *Breaker {
	b.now = now
	return b
}

// Allow reports whether a request may be sent. A caller that is allowed must
// later call Record exactly once.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = BreakerHalfOpen
		b.trial = true
		return true
	case BreakerHalfOpen:
		if b.trial {
			return false
		}
		b.trial = true
		return true
	default:
		return true
	}
}

// Record reports the outcome of an allowed request.
func (b *Breaker) Record(failure bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerHalfOpen {
		b.trial = false
		if failure {
			b.trip()
		} else {
			b.reset()
		}
		return
	}
	if b.state == BreakerOpen {
		return // a request allowed before the breaker opened
	}
	if b.filled == len(b.window) && b.window[b.next] {
		b.failures--
	}
	b.window[b.next] = failure
	if failure {
		b.failures++
	}
	b.next = (b.next + 1) % len(b.window)
	if b.filled < len(b.window) {
		b.filled++
	}
	if b.filled == len(b.window) && float64(b.failures) > b.threshold*float64(len(b.window)) {
		b.trip()
	}
}

// State returns the current state, moving to half-open if the cooldown has
// passed.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == BreakerOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		return BreakerHalfOpen
	}
	return b.state
}

func (b *Breaker) trip() {
	b.state = BreakerOpen
	b.openedAt = b.now()
}

func (b *Breaker) reset() {
	b.state = BreakerClosed
	clear(b.window)
	b.next, b.filled, b.failures = 0, 0, 0
}
