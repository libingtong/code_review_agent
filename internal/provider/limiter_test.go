package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func testConcurrency() config.ConcurrencyConfig {
	return config.ConcurrencyConfig{MaxInFlight: 8, InitialInFlight: 4, MinInFlight: 1,
		IncreaseAfterSuccesses: 3, IncreaseStep: 2, DecreaseFactor: 0.5, IncreaseCooldownSeconds: 0}
}

func TestLimiterCapsConcurrency(t *testing.T) {
	limiter := newLimiter(testConcurrency())
	if got := limiter.Current(); got != 4 {
		t.Fatalf("initial limit = %d, want 4", got)
	}
	var releases []func()
	for i := 0; i < 4; i++ {
		release, err := limiter.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if got := limiter.InFlight(); got != 4 {
		t.Fatalf("in flight = %d", got)
	}
	// The fifth acquisition must block until a slot frees.
	blocked := make(chan struct{})
	go func() {
		release, err := limiter.Acquire(context.Background())
		if err == nil {
			release()
		}
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("acquire must wait when the limit is reached")
	case <-time.After(30 * time.Millisecond):
	}
	releases[0]()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("released slot was not handed to the waiter")
	}
	for _, release := range releases[1:] {
		release()
	}
}

func TestLimiterHonoursContextCancellation(t *testing.T) {
	limiter := newLimiter(testConcurrency())
	for i := 0; i < 4; i++ {
		if _, err := limiter.Acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx); err == nil {
		t.Fatal("a cancelled context must not acquire a slot")
	}
}

func TestLimiterHalvesOnThrottleAndGrowsOnSuccess(t *testing.T) {
	limiter := newLimiter(testConcurrency())
	old, next := limiter.OnRateLimited()
	if old != 4 || next != 2 {
		t.Fatalf("throttle 4 -> %d (old %d), want 2", next, old)
	}
	old, next = limiter.OnRateLimited()
	if old != 2 || next != 1 {
		t.Fatalf("throttle should keep halving, got %d -> %d", old, next)
	}
	if !limiter.AtMinimum() {
		t.Fatal("limit 1 with min 1 must report the minimum")
	}
	// Below minimum is impossible.
	old, next = limiter.OnRateLimited()
	if next != 1 || old != 1 {
		t.Fatalf("limit must not drop below min: %d -> %d", old, next)
	}

	// Growth resumes only after enough successes.
	for i := 0; i < 2; i++ {
		if changed, _ := limiter.OnSuccess(); changed {
			t.Fatal("limit must not grow before the success threshold")
		}
	}
	changed, limit := limiter.OnSuccess()
	if !changed || limit != 3 {
		t.Fatalf("expected growth to 3, got changed=%v limit=%d", changed, limit)
	}
}

func TestLimiterGrowthStopsAtMax(t *testing.T) {
	cfg := testConcurrency()
	cfg.MaxInFlight = 4
	cfg.InitialInFlight = 3
	cfg.IncreaseAfterSuccesses = 1
	cfg.IncreaseStep = 4
	limiter := newLimiter(cfg)
	for i := 0; i < 5; i++ {
		limiter.OnSuccess()
	}
	if got := limiter.Current(); got != 4 {
		t.Fatalf("limit must not exceed max: %d", got)
	}
}

func TestLimiterCooldownBlocksGrowth(t *testing.T) {
	cfg := testConcurrency()
	cfg.IncreaseAfterSuccesses = 1
	cfg.IncreaseCooldownSeconds = 600
	now := time.Now()
	limiter := newLimiter(cfg)
	limiter.now = func() time.Time { return now }
	limiter.OnRateLimited()
	before := limiter.Current()
	if changed, limit := limiter.OnSuccess(); changed {
		t.Fatalf("growth during cooldown: %d", limit)
	}
	if limiter.Current() != before {
		t.Fatal("cooldown must keep the limit")
	}
}

// A burst of 429s must shrink concurrency and still deliver the request, not
// fail the run.
func TestRateLimitShrinksConcurrencyAndRecovers(t *testing.T) {
	var hits int32
	// Two 429s are enough to reach the floor of 1; the third attempt succeeds.
	server := newFlakyServer(t, &hits, 2)
	pool, err := NewPool(testConfig(testProvider("glm", server.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	pool.providers[0].limiter = newLimiter(testConcurrency())
	pool.SetWaiter(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	var changes []string
	pool.OnConcurrency(func(id string, from, to int, reason string) {
		changes = append(changes, id+": "+reason)
	})

	answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("rate limits must not fail the request: %v", err)
	}
	if answer != "ok" {
		t.Fatalf("answer = %q", answer)
	}
	current, max, ok := pool.ConcurrencyOf("glm")
	if !ok || current >= 4 || max != 8 {
		t.Fatalf("concurrency not reduced: current=%d max=%d ok=%v", current, max, ok)
	}
	status := pool.Statuses()[0]
	if status.Throttles == 0 {
		t.Fatal("throttle count not recorded")
	}
	if len(changes) == 0 {
		t.Fatal("concurrency change was not reported")
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("expected 2 failures then success, got %d requests", hits)
	}
}

// Once concurrency is at the floor and 429 persists, it is a plan limit: the
// request must surface as a resumable pause signal.
func TestPersistentRateLimitAtFloorBecomesQuotaPause(t *testing.T) {
	var hits int32
	server := always429Server(t, &hits)
	pool, err := NewPool(testConfig(testProvider("glm", server.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	pool.providers[0].limiter = newLimiter(config.ConcurrencyConfig{MaxInFlight: 1, InitialInFlight: 1, MinInFlight: 1,
		IncreaseAfterSuccesses: 1, IncreaseStep: 1, DecreaseFactor: 0.5})
	pool.SetWaiter(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err == nil {
		t.Fatal("persistent 429 must surface as an error")
	} else if !errors.Is(err, ErrAllExhausted) {
		t.Fatalf("want ErrAllExhausted, got %v", err)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("should stop after the hard cap, got %d requests", hits)
	}
}

// newFlakyServer fails with 429 for the first fails requests, then answers.
func newFlakyServer(t *testing.T, hits *int32, fails int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(hits, 1) <= fails {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"429","message":"rate limit exceeded, retry-after: 5"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func always429Server(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"429","message":"rate limit exceeded"}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// newSlowServer tracks peak in-flight requests so the limiter can be verified.
func newSlowServer(t *testing.T, inFlight, peak *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := atomic.AddInt32(inFlight, 1)
		for {
			previous := atomic.LoadInt32(peak)
			if current <= previous || atomic.CompareAndSwapInt32(peak, previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(inFlight, -1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// The pool must never exceed the live limit, and must recover it after a calm
// period of successes.
func TestInFlightNeverExceedsLimit(t *testing.T) {
	var inFlight, peak int32
	server := newSlowServer(t, &inFlight, &peak)
	pool, err := NewPool(testConfig(testProvider("glm", server.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	pool.providers[0].limiter = newLimiter(config.ConcurrencyConfig{MaxInFlight: 3, InitialInFlight: 3, MinInFlight: 1,
		IncreaseAfterSuccesses: 1000, IncreaseStep: 1, DecreaseFactor: 0.5})

	var wg sync.WaitGroup
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&peak); got > 3 {
		t.Fatalf("peak concurrency %d exceeded the limit 3", got)
	}
	if current, _, ok := pool.ConcurrencyOf("glm"); !ok || current != 3 {
		t.Fatalf("limit drifted without throttling: %d ok=%v", current, ok)
	}
}

func TestRoutingNotesReachTheUI(t *testing.T) {
	a := chatServer(t, "a")
	b := chatServer(t, "b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if notes := pool.DrainNotes(); len(notes) != 0 {
		t.Fatalf("a fresh pool has no notes: %v", notes)
	}
	if err := pool.SetActive("b"); err != nil {
		t.Fatal(err)
	}
	notes := pool.DrainNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "a → b") {
		t.Fatalf("manual switch must be reported: %v", notes)
	}
	if again := pool.DrainNotes(); len(again) != 0 {
		t.Fatalf("notes must be drained once: %v", again)
	}
}

func TestQuotaSummaryShowsThrottling(t *testing.T) {
	a := chatServer(t, "a")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 100)))
	if err != nil {
		t.Fatal(err)
	}
	pool.providers[0].limiter = newLimiter(config.ConcurrencyConfig{MaxInFlight: 8, InitialInFlight: 4,
		MinInFlight: 1, IncreaseAfterSuccesses: 100, IncreaseStep: 1, DecreaseFactor: 0.5})
	if summary := pool.QuotaSummary(); !strings.Contains(summary, "并发4/8") {
		t.Fatalf("summary must show the reduced concurrency: %q", summary)
	}
}

// A stable limit is learned once and reused on the next start, so a fresh run
// does not re-trigger the 429s that were already solved.
func TestLimiterLearnsStableConcurrency(t *testing.T) {
	cfg := testConcurrency()
	cfg.SettleSuccesses = 5
	cfg.IncreaseAfterSuccesses = 1000 // growth off: we only test learning
	limiter := newLimiter(cfg)
	var learned []int
	limiter.OnSettle(func(limit int) { learned = append(learned, limit) })

	for i := 0; i < 5; i++ {
		limiter.OnSuccess()
	}
	if limiter.Learned() != 4 {
		t.Fatalf("learned = %d, want 4", limiter.Learned())
	}
	if len(learned) != 1 || learned[0] != 4 {
		t.Fatalf("settle callback = %v", learned)
	}
	// Further successes at the same limit do not re-report.
	for i := 0; i < 10; i++ {
		limiter.OnSuccess()
	}
	if len(learned) != 1 {
		t.Fatalf("duplicate settle callback: %v", learned)
	}
}

func TestLimiterRelearnsLowerValueAfterThrottle(t *testing.T) {
	cfg := testConcurrency()
	cfg.SettleSuccesses = 3
	cfg.IncreaseAfterSuccesses = 1000
	limiter := newLimiter(cfg)
	limiter.OnRateLimited() // 4 -> 2
	for i := 0; i < 3; i++ {
		limiter.OnSuccess()
	}
	if limiter.Learned() != 2 {
		t.Fatalf("learned = %d, want 2 after throttling", limiter.Learned())
	}
}

func TestLimiterPinStopsGrowthAtLearned(t *testing.T) {
	cfg := testConcurrency()
	cfg.SettleSuccesses = 2
	cfg.IncreaseAfterSuccesses = 1
	cfg.IncreaseStep = 4
	cfg.Pin = true
	limiter := newLimiter(cfg)
	// A pinned channel starts from the value that proved stable last time.
	limiter.SetLearned(4)
	for i := 0; i < 10; i++ {
		limiter.OnSuccess()
	}
	if limiter.Current() > limiter.Learned() {
		t.Fatalf("pinned limiter grew: current=%d learned=%d", limiter.Current(), limiter.Learned())
	}
}

func TestConcurrencyStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/concurrency.json"

	a := chatServer(t, "a")
	cfg := testConcurrency()
	cfg.StateFile = path
	cfg.SettleSuccesses = 2
	cfg.IncreaseAfterSuccesses = 1000
	// Both pools need the same ceiling, otherwise SetLearned clamps to a smaller
	// default max and the round trip looks broken.
	base := testConfig(testProvider("glm", a.URL, 0))
	base.Concurrency = cfg
	pool, err := NewPool(base)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetConcurrencyStatePath(path)
	pool.providers[0].limiter = newLimiter(cfg)
	pool.providers[0].limiter.OnSettle(func(limit int) { pool.noteSettle("glm", limit) })
	for i := 0; i < 2; i++ {
		pool.providers[0].limiter.OnSuccess()
	}
	if got := pool.LearnedConcurrency("glm"); got != 4 {
		t.Fatalf("learned in memory = %d", got)
	}

	reloaded, err := NewPool(base)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetConcurrencyStatePath(path)
	if err := reloaded.LoadConcurrencyState(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.LearnedConcurrency("glm"); got != 4 {
		t.Fatalf("learned not restored: %d", got)
	}
	if current, _, _ := reloaded.ConcurrencyOf("glm"); current != 4 {
		t.Fatalf("restored limiter must start at the learned value: %d", current)
	}
	if status := reloaded.Statuses()[0]; status.Learned != 4 {
		t.Fatalf("status must expose learned value: %+v", status)
	}

	// Forgetting it clears both memory and file.
	if err := reloaded.ResetLearned("glm"); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.LearnedConcurrency("glm"); got != 0 {
		t.Fatalf("learned not cleared: %d", got)
	}
	if err := reloaded.ResetLearned("nope"); err == nil {
		t.Fatal("unknown channel must fail")
	}
}
