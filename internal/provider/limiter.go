package provider

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"code-review-agent/internal/config"
)

// Limiter is an adaptive in-flight request limiter (AIMD).
//
// Subscription plans advertise small concurrent-project limits; a team of 12
// audit agents easily exceeds them, which shows up as a burst of 429s. Instead
// of failing the whole run, the pool shrinks its concurrency for that source and
// grows it back slowly while requests succeed.
//
// It limits *requests*, not workers: an agent waiting for a slot is still alive
// with its context intact, so no work is lost.
type Limiter struct {
	mu        sync.Mutex
	limit     float64
	max       int
	min       int
	inFlight  int
	waiters   []chan struct{}
	successes int
	cfg       config.ConcurrencyConfig
	now       func() time.Time
	// lastDecrease blocks growth for a while after a throttle event.
	lastDecrease time.Time
	// throttles counts 429s so callers can distinguish "concurrency too high"
	// from "this plan is actually out of quota".
	throttles int
	lastLimit int
}

func newLimiter(cfg config.ConcurrencyConfig) *Limiter {
	limit := cfg.InitialInFlight
	if limit <= 0 {
		limit = minInt(4, cfg.MaxInFlight)
	}
	if limit < 1 {
		limit = 1
	}
	return &Limiter{limit: float64(limit), max: cfg.MaxInFlight, min: cfg.MinInFlight,
		cfg: cfg, now: time.Now, lastLimit: limit}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Acquire blocks until a slot is free or ctx ends. The returned release function
// is idempotent so a deferred call after an error path is safe.
func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		l.mu.Lock()
		if l.inFlight < int(l.limit) {
			l.inFlight++
			l.mu.Unlock()
			var once sync.Once
			return func() { once.Do(l.release) }, nil
		}
		wait := make(chan struct{}, 1)
		l.waiters = append(l.waiters, wait)
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			l.mu.Lock()
			for i, candidate := range l.waiters {
				if candidate == wait {
					l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
					break
				}
			}
			l.mu.Unlock()
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

func (l *Limiter) release() {
	l.mu.Lock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	l.notifyLocked()
	l.mu.Unlock()
}

func (l *Limiter) notifyLocked() {
	waiters := l.waiters
	l.waiters = nil
	for _, waiter := range waiters {
		// Buffered, non-blocking: the waiter re-checks the limit itself.
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
}

// Current is the limit currently in force.
func (l *Limiter) Current() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.limit)
}

// InFlight is the number of requests currently executing.
func (l *Limiter) InFlight() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight
}

func (l *Limiter) Max() int { return l.max }

func (l *Limiter) Min() int { return l.min }

func (l *Limiter) Throttles() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.throttles
}

// AtMinimum reports that concurrency has already been squeezed as far as it
// goes; further 429s therefore mean quota, not parallelism.
func (l *Limiter) AtMinimum() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.limit) <= l.min
}

// OnSuccess grows the limit after enough consecutive successes.
func (l *Limiter) OnSuccess() (changed bool, limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successes++
	cooldown := time.Duration(l.cfg.IncreaseCooldownSeconds) * time.Second
	if int(l.limit) >= l.max || l.successes < l.cfg.IncreaseAfterSuccesses {
		return false, int(l.limit)
	}
	if cooldown > 0 && !l.lastDecrease.IsZero() && l.now().Sub(l.lastDecrease) < cooldown {
		return false, int(l.limit)
	}
	l.successes = 0
	l.limit = math.Min(float64(l.max), l.limit+float64(l.cfg.IncreaseStep))
	l.lastLimit = int(l.limit)
	l.notifyLocked()
	return true, int(l.limit)
}

// OnRateLimited halves the limit (never below min) and returns the transition.
func (l *Limiter) OnRateLimited() (old, next int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old = int(l.limit)
	l.throttles++
	l.successes = 0
	l.lastDecrease = l.now()
	scaled := math.Floor(float64(old) * l.cfg.DecreaseFactor)
	next = int(scaled)
	if next < l.min {
		next = l.min
	}
	if next >= old {
		// A decrease factor close to 1 still has to make progress.
		next = old - 1
		if next < l.min {
			next = l.min
		}
	}
	l.limit = float64(next)
	l.lastLimit = next
	l.notifyLocked()
	return old, next
}

// Reset restores the initial concurrency, used when the plan window refreshes.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = float64(l.cfg.InitialInFlight)
	if l.limit < float64(l.min) {
		l.limit = float64(l.min)
	}
	l.successes = 0
	l.throttles = 0
	l.lastDecrease = time.Time{}
	l.notifyLocked()
}

// Describe renders the limiter for the status line.
func (l *Limiter) Describe() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprintf("并发 %d/%d（自适应）", int(l.limit), l.max)
}
