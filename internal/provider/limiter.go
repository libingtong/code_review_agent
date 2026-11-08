package provider

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"code-review-agent/internal/config"
)

// rateLimitDecreaseCooldown coalesces a burst of 429s. Vendors throttle every
// request that was already in flight when the quota window closed, so five
// simultaneous 429s used to halve the limit five times (17→8→4→2→1 in 400ms) and
// serialize every later request of the run.
const rateLimitDecreaseCooldown = 20 * time.Second

// learnedFloor is the lowest concurrency worth remembering between runs. A burst
// collapse can leave the limit at 1; persisting that would cap every future run
// even though the plan was only throttled at 17.
const learnedFloor = 4

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
	// learned is the concurrency that survived a settle window; the next start
	// uses it instead of re-probing from initial_in_flight.
	learned int
	// stable counts successes since the last limit change, for settle detection.
	stable int
	// onSettle reports a newly learned value so the pool can persist it.
	onSettle func(limit int)
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
		cfg: cfg, now: time.Now, lastLimit: limit, learned: 0}
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

// OnSuccess grows the limit after enough consecutive successes and, once the
// limit has held still for settle_successes, reports it as the learned value.
func (l *Limiter) OnSuccess() (changed bool, limit int) {
	l.mu.Lock()
	l.successes++
	l.stable++
	settle := l.cfg.LearnsConcurrency() && l.cfg.SettleSuccesses > 0 && l.stable >= l.cfg.SettleSuccesses &&
		int(l.limit) != l.learned && int(l.limit) >= learnedFloor
	learnedNow := 0
	if settle {
		l.learned = int(l.limit)
		l.stable = 0
		learnedNow = l.learned
	}
	grow := l.growthAllowedLocked()
	var next int
	if grow {
		l.successes = 0
		l.limit = math.Min(float64(l.max), l.limit+float64(l.cfg.IncreaseStep))
		l.lastLimit = int(l.limit)
		next = int(l.limit)
		l.stable = 0
		l.notifyLocked()
	}
	callback := l.onSettle
	l.mu.Unlock()
	if learnedNow > 0 && callback != nil {
		callback(learnedNow)
	}
	return grow, next
}

// growthAllowedLocked decides whether additive increase may run. A pinned
// learned value stops growth at the value that proved stable.
func (l *Limiter) growthAllowedLocked() bool {
	if int(l.limit) >= l.max || l.successes < l.cfg.IncreaseAfterSuccesses {
		return false
	}
	if l.cfg.Pin && l.learned > 0 && int(l.limit) >= l.learned {
		return false
	}
	cooldown := time.Duration(l.cfg.IncreaseCooldownSeconds) * time.Second
	if cooldown > 0 && !l.lastDecrease.IsZero() && l.now().Sub(l.lastDecrease) < cooldown {
		return false
	}
	return true
}

// SetLearned applies a persisted value: the next run starts here.
func (l *Limiter) SetLearned(value int) {
	if value <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if value < l.min {
		value = l.min
	}
	// Never revive a value that only ever came out of a burst collapse.
	if value < learnedFloor && !l.cfg.Pin {
		value = learnedFloor
	}
	if value > l.max {
		value = l.max
	}
	l.learned = value
	l.limit = float64(value)
	l.stable = 0
	l.successes = 0
	l.notifyLocked()
}

// Learned reports the remembered value (0 when nothing has settled yet).
func (l *Limiter) Learned() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.learned
}

// OnSettle installs the persistence callback.
func (l *Limiter) OnSettle(fn func(limit int)) {
	l.mu.Lock()
	l.onSettle = fn
	l.mu.Unlock()
}

// OnRateLimited halves the limit (never below min) and returns the transition.
func (l *Limiter) OnRateLimited() (old, next int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old = int(l.limit)
	l.throttles++
	l.successes = 0
	l.stable = 0
	now := l.now()
	if !l.lastDecrease.IsZero() && now.Sub(l.lastDecrease) < rateLimitDecreaseCooldown {
		// Same throttle window: the limit already paid for this burst. Count the
		// throttle (callers use it to detect "plan exhausted") but do not shrink
		// again.
		return old, old
	}
	l.lastDecrease = now
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

// Reset restores the starting concurrency: the learned value when one exists,
// otherwise initial_in_flight. Used by /concurrency reset.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.learned > 0 {
		l.limit = float64(l.learned)
	} else {
		l.limit = float64(l.cfg.InitialInFlight)
	}
	if l.limit < float64(l.min) {
		l.limit = float64(l.min)
	}
	l.successes = 0
	l.stable = 0
	l.throttles = 0
	l.lastDecrease = time.Time{}
	l.notifyLocked()
}

// ForgetLearned drops the remembered value and resets to the configured start.
func (l *Limiter) ForgetLearned() {
	l.mu.Lock()
	l.learned = 0
	l.limit = float64(l.cfg.InitialInFlight)
	if l.limit < float64(l.min) {
		l.limit = float64(l.min)
	}
	l.stable = 0
	l.successes = 0
	l.notifyLocked()
	l.mu.Unlock()
}

// Describe renders the limiter for the status line.
func (l *Limiter) Describe() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.learned > 0 {
		return fmt.Sprintf("并发 %d/%d（学习值 %d）", int(l.limit), l.max, l.learned)
	}
	return fmt.Sprintf("并发 %d/%d（自适应）", int(l.limit), l.max)
}
