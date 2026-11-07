package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func testProvider(id, baseURL string, quotaTotal float64) config.ProviderConfig {
	enabled := true
	return config.ProviderConfig{
		ID:      id,
		Label:   strings.ToUpper(id),
		Enabled: &enabled,
		Weight:  1,
		OpenAI: config.OpenAIConfig{
			BaseURL:          baseURL,
			APIInterface:     "chat",
			APIKey:           "sk-" + id,
			Model:            "model-" + id,
			MaxContextTokens: 1000,
			MaxOutputTokens:  100,
			TimeoutSeconds:   5,
			TopP:             1,
		},
		Quota: config.QuotaConfig{
			Window: "5h",
			Local:  config.LocalQuotaConfig{Unit: "credits", Total: quotaTotal},
		},
	}
}

func testConfig(providers ...config.ProviderConfig) config.Config {
	return config.Config{Providers: providers, Routing: config.RoutingConfig{SwitchBelow: 0.15, MinDwellSeconds: 300, Prefer: "sticky"}}
}

// chatServer answers Chat Completions with a fixed message.
func chatServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, content)))
	}))
	t.Cleanup(server.Close)
	return server
}

func failingServer(t *testing.T, status int, message string, hits *int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":"%d","message":%q}}`, status, message)))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPoolRoutesToActiveProvider(t *testing.T) {
	a := chatServer(t, "from-a")
	b := chatServer(t, "from-b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if pool.ActiveID() != "a" {
		t.Fatalf("active = %q, want a", pool.ActiveID())
	}
	answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "from-a" {
		t.Fatalf("answer = %q, want from-a", answer)
	}
}

func TestBillingFailureSwitchesSource(t *testing.T) {
	var hits int32
	a := failingServer(t, http.StatusPaymentRequired, "insufficient balance 余额不足", &hits)
	b := chatServer(t, "from-b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	var switchMu sync.Mutex
	var switched []string
	pool.OnSwitch(func(from, to, reason string) {
		switchMu.Lock()
		switched = append(switched, from+"->"+to)
		switchMu.Unlock()
	})
	snapshotSwitches := func() []string {
		switchMu.Lock()
		defer switchMu.Unlock()
		return append([]string(nil), switched...)
	}

	answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("failover should have produced an answer: %v", err)
	}
	if answer != "from-b" {
		t.Fatalf("answer = %q", answer)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("failing source hit %d times, want exactly 1", hits)
	}
	statuses := pool.Statuses()
	// A single unconfirmed 402 parks the channel briefly instead of draining the
	// whole 5h window, so either signal is acceptable here.
	if !statuses[0].Exhausted && !statuses[0].CooldownTill.After(time.Now()) {
		t.Fatalf("source a must be parked or exhausted: %+v", statuses[0])
	}
	if statuses[0].LastError == "" {
		t.Fatal("failure reason must be recorded for /providers")
	}
	waitFor(t, func() bool { return len(snapshotSwitches()) >= 1 })
	if got := snapshotSwitches()[0]; got != "a->b" {
		t.Fatalf("switch reason path wrong: %q", got)
	}
}

func TestAllSourcesExhaustedIsAPauseNotACrash(t *testing.T) {
	a := failingServer(t, http.StatusPaymentRequired, "insufficient balance", nil)
	b := failingServer(t, http.StatusTooManyRequests, "rate limit exceeded", nil)
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	// Keep the 429 backoff instant so the test does not sleep for minutes.
	pool.SetWaiter(func(ctx context.Context, _ time.Duration) error { return ctx.Err() })
	_, err = pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if !errors.Is(err, ErrAllExhausted) {
		t.Fatalf("want ErrAllExhausted, got %v", err)
	}
	budget := pool.EarliestReset()
	if budget.EarliestReset.IsZero() {
		t.Fatal("a paused pool must expose the earliest reset time")
	}
}

func TestNetworkFailureDoesNotSwitchSource(t *testing.T) {
	// 500 is transport/service trouble: retrying elsewhere would hide an outage
	// behind a second vendor's quota.
	a := failingServer(t, http.StatusInternalServerError, "server error", nil)
	b := chatServer(t, "from-b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err == nil {
		t.Fatal("server error must surface to the retry layer, not silently switch")
	}
	if pool.ActiveID() != "a" {
		t.Fatalf("active changed to %q on a non-quota failure", pool.ActiveID())
	}
}

func TestSetEnabledRequiresAnotherSource(t *testing.T) {
	a := chatServer(t, "a")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.SetEnabled("a", false); err == nil {
		t.Fatal("disabling the only enabled source must fail")
	}
	if err := pool.SetEnabled("missing", false); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

func TestContinuationDroppedWhenProviderChanges(t *testing.T) {
	a := chatServer(t, "a")
	b := chatServer(t, "b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", b.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	pool.rememberLineage("resp-1", "a")
	ctx := llm.WithResponsesContinuation(context.Background(), "resp-1", nil)

	if got := llm.ResponseContinuationID(pool.affinityContext(ctx, "a")); got != "resp-1" {
		t.Fatalf("same-provider continuation must survive, got %q", got)
	}
	if got := llm.ResponseContinuationID(pool.affinityContext(ctx, "b")); got != "" {
		t.Fatalf("cross-provider continuation must be dropped, got %q", got)
	}
}

func TestQuotaSummaryAndLines(t *testing.T) {
	a := chatServer(t, "a")
	b := chatServer(t, "b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 100), testProvider("b", b.URL, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if summary := pool.QuotaSummary(); !strings.Contains(summary, "*a") || !strings.Contains(summary, "100%") {
		t.Fatalf("summary = %q", summary)
	}
	lines := pool.QuotaLines()
	if len(lines) != 2 || !strings.Contains(lines[0], "▸ ") {
		t.Fatalf("lines = %v", lines)
	}
}

func TestUsageIsChargedToTheServingSource(t *testing.T) {
	a := chatServer(t, "a")
	b := chatServer(t, "b")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 1000), testProvider("b", b.URL, 1000)))
	if err != nil {
		t.Fatal(err)
	}
	// Intermediate estimates are ignored; only the final total is billed.
	pool.chargeUsage("a", llm.UsageUpdate{RequestID: 1, TotalTokens: 500})
	pool.chargeUsage("a", llm.UsageUpdate{RequestID: 1, TotalTokens: 1000, Final: true})
	statuses := pool.Statuses()
	if statuses[0].Used == 0 {
		t.Fatal("usage was not charged to the serving source")
	}
	if statuses[1].Used != 0 {
		t.Fatalf("usage leaked to the idle source: %v", statuses[1].Used)
	}
}

func TestQuotaStateRoundTrip(t *testing.T) {
	a := chatServer(t, "a")
	dir := t.TempDir()
	path := dir + "/quota.json"
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 500)))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetQuotaStatePath(path)
	pool.chargeUsage("a", llm.UsageUpdate{RequestID: 7, TotalTokens: 2_000_000, Final: true})
	before := pool.Statuses()[0]
	if err := pool.SaveQuotaState(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewPool(testConfig(testProvider("a", a.URL, 500)))
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetQuotaStatePath(path)
	if err := reloaded.LoadQuotaState(); err != nil {
		t.Fatal(err)
	}
	after := reloaded.Statuses()[0]
	if after.Used != before.Used || after.ResetAt.Unix() != before.ResetAt.Unix() {
		t.Fatalf("window state not restored: before=%+v after=%+v", before, after)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if condition() {
			return
		}
		timeSleep()
	}
	t.Fatal("condition not reached")
}

var _ = json.Marshal

func timeSleep() { time.Sleep(5 * time.Millisecond) }

// A provider without a key must not block the run: it is skipped with a note,
// which is the normal "add the vendor now, paste the key later" flow.
func TestKeylessProviderIsSkippedWhenAnotherIsUsable(t *testing.T) {
	ready := chatServer(t, "from-ready")
	keyed := testProvider("mm", ready.URL, 0)
	keyless := testProvider("glm", "https://example.invalid/v1", 0)
	keyless.OpenAI.APIKey = ""
	keyless.OpenAI.APIKeyEnv = "GLM_API_KEY"

	pool, err := NewPool(testConfig(keyed, keyless))
	if err != nil {
		t.Fatal(err)
	}
	statuses := pool.Statuses()
	if statuses[1].Enabled {
		t.Fatal("a keyless source must be disabled")
	}
	if statuses[0].Enabled != true || pool.ActiveID() != "mm" {
		t.Fatalf("usable source should stay active: %+v", statuses)
	}
	notes := pool.DrainNotes()
	if len(notes) == 0 || !strings.Contains(notes[0], "GLM_API_KEY") {
		t.Fatalf("skip reason must name the env var: %v", notes)
	}
	if answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err != nil || answer != "from-ready" {
		t.Fatalf("run must proceed on the usable source: %q %v", answer, err)
	}
}

func TestAllKeylessProvidersStillStart(t *testing.T) {
	a := testProvider("a", "https://example.invalid/v1", 0)
	a.OpenAI.APIKey = ""
	b := testProvider("b", "https://example.invalid/v1", 0)
	b.OpenAI.APIKey = ""
	pool, err := NewPool(testConfig(a, b))
	if err != nil {
		t.Fatalf("a fully keyless config must still build (keys are filled later): %v", err)
	}
	if pool.ActiveID() == "" {
		t.Fatal("an active source must be selected")
	}
}

// A channel whose working-hours policy is closed must not be selected, even if
// it has plenty of quota.
type fixedClock struct {
	open map[string]bool
}

func (f fixedClock) OpenAt(channel string, at time.Time) bool { return f.open[channel] }

func TestChannelScheduleGatesRouting(t *testing.T) {
	night := chatServer(t, "night")
	library := chatServer(t, "library")
	a := testProvider("night", night.URL, 0)
	b := testProvider("library", library.URL, 0)
	b.Weight = 5 // would win on weight alone
	pool, err := NewPool(testConfig(a, b))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetChannelClock(fixedClock{open: map[string]bool{"library": true}})

	if statuses := pool.Statuses(); statuses[0].Scheduled || !statuses[1].Scheduled {
		t.Fatalf("schedule state not reported: %+v", statuses)
	}
	chosen := pool.nextProvider(map[string]bool{})
	if chosen == nil || chosen.ID() != "library" {
		t.Fatalf("router ignored the channel schedule, chose %v", chosen)
	}
	answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil || answer != "library" {
		t.Fatalf("request should use the open channel: %q %v", answer, err)
	}

	// Everything closed: no provider is ready, which the gate turns into a pause.
	pool.SetChannelClock(fixedClock{open: map[string]bool{}})
	if pool.hasReadyProvider() {
		t.Fatal("no channel is open, nothing may be ready")
	}
	if next := pool.nextProvider(map[string]bool{}); next != nil {
		t.Fatalf("router picked a closed channel: %s", next.ID())
	}
}

// The reported case: one channel is out of hours AND out of quota, the other is
// always available. The request must move to the available channel.
func TestFailsOverWhenOneChannelIsOutOfHoursAndCapped(t *testing.T) {
	mmServer := chatServer(t, "from-minimax")
	glmServer := chatServer(t, "from-glm")
	mm := testProvider("minimax", mmServer.URL, 100)
	mm.Quota.Local.Unit = "tokens"
	mm.Quota.MaxUsageRatio = 0.5
	glm := testProvider("glm", glmServer.URL, 0)

	pool, err := NewPool(testConfig(mm, glm))
	if err != nil {
		t.Fatal(err)
	}
	pool.chargeUsage("minimax", llm.UsageUpdate{RequestID: 1, TotalTokens: 60, Final: true})
	// minimax is outside its window; glm is always available.
	pool.SetChannelClock(fixedClock{open: map[string]bool{"glm": true}})

	answer, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("must fail over to the available channel: %v", err)
	}
	if answer != "from-glm" {
		t.Fatalf("answer came from %q, want from-glm", answer)
	}
	if statuses := pool.Statuses(); statuses[1].Active != true {
		t.Fatalf("active channel should be glm: %+v", statuses)
	}
}

// When every channel is closed by policy the error must say so, not claim that
// quota ran out.
func TestAllChannelsOutOfHoursReportsPolicyNotQuota(t *testing.T) {
	a := chatServer(t, "a")
	pool, err := NewPool(testConfig(testProvider("a", a.URL, 0), testProvider("b", a.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetChannelClock(fixedClock{open: map[string]bool{}})
	_, err = pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
	if !errors.Is(err, ErrOutOfHours) {
		t.Fatalf("want ErrOutOfHours, got %v", err)
	}
	if errors.Is(err, ErrAllExhausted) {
		t.Fatalf("a policy pause must not be reported as quota exhaustion: %v", err)
	}
	if !strings.Contains(err.Error(), "时段") {
		t.Fatalf("message must name the working-hours policy: %v", err)
	}
}

// A channel disabled for a missing key must be named in the diagnosis, otherwise
// a failed failover looks inexplicable.
func TestExhaustionReportNamesDisabledChannels(t *testing.T) {
	a := chatServer(t, "a")
	keyed := testProvider("minimax", a.URL, 0)
	keyless := testProvider("glm", "https://example.invalid/v1", 0)
	keyless.OpenAI.APIKey = ""
	keyless.OpenAI.APIKeyEnv = "GLM_API_KEY"

	pool, err := NewPool(testConfig(keyed, keyless))
	if err != nil {
		t.Fatal(err)
	}
	report := pool.ExhaustionReport()
	if !strings.Contains(report, "glm") || !strings.Contains(report, "已停用") || !strings.Contains(report, "GLM_API_KEY") {
		t.Fatalf("disabled channel not explained: %q", report)
	}
	if !strings.Contains(report, "minimax") {
		t.Fatalf("enabled channel missing from the report: %q", report)
	}
}
