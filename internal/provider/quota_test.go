package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func windowFor(t *testing.T, total float64) *QuotaWindow {
	t.Helper()
	entry := testProvider("glm", "http://example.invalid", total)
	// Token units keep the accounting assertions exact; credits are covered by
	// TestChargeForUsesOffPeakDiscount.
	entry.Quota.Local.Unit = "tokens"
	window, err := newQuotaWindow(entry)
	if err != nil {
		t.Fatal(err)
	}
	return window
}

func TestWindowStartsOnFirstUseAndRollsAfterFiveHours(t *testing.T) {
	window := windowFor(t, 1000)
	start := time.Date(2026, 11, 7, 21, 0, 0, 0, time.UTC)

	if ratio := window.Ratio(start); ratio != 1 {
		t.Fatalf("idle window must look full, got %v", ratio)
	}
	window.RecordUsage(200, start)
	status := window.Status(start)
	if !status.ResetAt.Equal(start.Add(5 * time.Hour)) {
		t.Fatalf("reset = %v, want start+5h", status.ResetAt)
	}
	if status.Used != 200 || status.Ratio != 0.8 {
		t.Fatalf("unexpected accounting: %+v", status)
	}

	// Unused quota does not accumulate: after the reset instant the window is
	// idle again, not "500 remaining".
	window.RecordUsage(100, start.Add(5*time.Hour+time.Minute))
	status = window.Status(start.Add(5*time.Hour + time.Minute))
	if status.Used != 100 {
		t.Fatalf("window should have rolled over, used=%v", status.Used)
	}
	if !status.ResetAt.Equal(start.Add(10*time.Hour + time.Minute)) {
		t.Fatalf("new window must reset 5h after its first use, got %v", status.ResetAt)
	}
}

func TestWindowExhaustionAndRecovery(t *testing.T) {
	window := windowFor(t, 100)
	now := time.Now()
	window.RecordUsage(150, now)
	status := window.Status(now)
	if !status.Exhausted {
		t.Fatal("over-budget usage must exhaust the window")
	}
	if window.Ready(now) {
		t.Fatal("an exhausted window must not be ready")
	}
	if ratio := window.Ratio(now); ratio != 0 {
		t.Fatalf("exhausted ratio = %v", ratio)
	}
	later := now.Add(6 * time.Hour)
	if !window.Ready(later) {
		t.Fatal("window must become ready again after its reset instant")
	}
}

func TestMinHeadroomKeepsSourceOutOfRotationEarly(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 100)
	entry.Quota.Local.Unit = "tokens"
	entry.Quota.MinHeadroom = 0.25
	window, err := newQuotaWindow(entry)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	window.RecordUsage(80, now) // 20% left, below the 25% headroom
	if window.Ready(now) {
		t.Fatal("source must stop accepting work once below min_headroom")
	}
	if ratio := window.Ratio(now); math.Abs(ratio-0.2) > 1e-9 {
		t.Fatalf("ratio = %v", ratio)
	}
}

func TestUnknownTotalNeverBlocksWork(t *testing.T) {
	window := windowFor(t, 0)
	now := time.Now()
	window.RecordUsage(10_000_000, now)
	if !window.Ready(now) {
		t.Fatal("an unmeasured vendor must stay usable until it reports exhaustion")
	}
	if window.Status(now).Ratio != 1 {
		t.Fatal("unknown total must report full ratio")
	}
	// A passive vendor signal still pauses it.
	window.markExhausted(now, now.Add(time.Hour), "429")
	if window.Ready(now) {
		t.Fatal("passive exhaustion must be honoured")
	}
}

func TestProbeReadingOverridesLocalEstimate(t *testing.T) {
	window := windowFor(t, 1000)
	now := time.Now()
	window.RecordUsage(900, now)
	reset := now.Add(2 * time.Hour)
	window.applyProbe(Sample{Used: 120, Total: 2000, ResetAt: reset, HasUsed: true, HasTotal: true, HasReset: true}, now, "credits")
	status := window.Status(now)
	if status.Used != 120 || status.Total != 2000 || !status.ResetAt.Equal(reset) {
		t.Fatalf("probe reading not applied: %+v", status)
	}
	if status.Source != "probe" {
		t.Fatalf("source = %q", status.Source)
	}
}

func TestChargeForUsesOffPeakDiscount(t *testing.T) {
	local := config.LocalQuotaConfig{Unit: "credits", CreditsPerMillionTokens: 1545, OffpeakFactor: 0.5, PeakFactor: 1,
		Peak: &config.PeakConfig{Days: []string{"mon"}, Start: "14:00", End: "18:00"}}
	mondayPeak := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	mondayNight := time.Date(2026, 11, 9, 23, 0, 0, 0, time.UTC)
	if got := chargeFor(local, 1_000_000, mondayPeak); math.Abs(got-1545) > 1e-6 {
		t.Fatalf("peak charge = %v", got)
	}
	if got := chargeFor(local, 1_000_000, mondayNight); math.Abs(got-772.5) > 1e-6 {
		t.Fatalf("off-peak charge = %v", got)
	}
	tokens := config.LocalQuotaConfig{Unit: "tokens"}
	if got := chargeFor(tokens, 4242, mondayPeak); got != 4242 {
		t.Fatalf("token unit charge = %v", got)
	}
}

func TestResetHintParsing(t *testing.T) {
	cases := map[string]int{
		"rate limited; retry-after: 120": 120,
		"quota exceeded, resets in 3600": 3600,
		"no hint here":                   0,
		"retry_after=95 seconds":         95,
	}
	for text, want := range cases {
		got, ok := resetHint(errText(text))
		if want == 0 {
			if ok {
				t.Fatalf("%q: unexpected hint %d", text, got)
			}
			continue
		}
		if !ok || got != want {
			t.Fatalf("%q: got %d ok=%v, want %d", text, got, ok, want)
		}
	}
}

type errText string

func (e errText) Error() string { return string(e) }

func TestRefreshQuotaAppliesProbeAndRecordsRaw(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 0)
	entry.Quota.Probe = config.ProbeConfig{Type: "local"}
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.RefreshQuota(context.Background(), "glm"); err == nil {
		t.Fatal("local probe should report that it has no external reading")
	}
	if _, _, err := pool.RefreshQuota(context.Background(), "nope"); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

// A 90% cap leaves the last 10% of the window for other tools on the same
// subscription, which is the whole point of the setting.
func TestUsageCapStopsAtConfiguredRatio(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 1000)
	entry.Quota.Local.Unit = "tokens"
	entry.Quota.MaxUsageRatio = 0.9
	window, err := newQuotaWindow(entry)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	window.RecordUsage(400, now)
	if !window.Ready(now) {
		t.Fatal("below the cap the source must stay usable")
	}
	window.RecordUsage(500, now) // 900 of 1000 = exactly the cap
	status := window.Status(now)
	if !status.SelfLimited || !status.Capped {
		t.Fatalf("cap not reported: %+v", status)
	}
	if status.Usable != 0 || status.Ratio != 0 {
		t.Fatalf("usable should be zero at the cap: %+v", status)
	}
	if window.Ready(now) {
		t.Fatal("a capped source must not accept new work")
	}
	if window.Capped(now) != true {
		t.Fatal("Capped() must agree with Status")
	}
	// The vendor quota itself is not exhausted: 100 units remain untouched.
	if status.Exhausted {
		t.Fatal("reaching the self-imposed cap is not vendor exhaustion")
	}
	if status.Remaining != 100 {
		t.Fatalf("remaining vendor quota = %v, want 100", status.Remaining)
	}
}

func TestUsageCapReserveUnits(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 12000)
	entry.Quota.Local.Unit = "credits"
	// Pin the rate so the assertion is about reserve units, not about pricing.
	entry.Quota.Local.CreditsPerMillionTokens = 1000
	entry.Quota.ReserveUnits = 2000
	window, err := newQuotaWindow(entry)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	window.RecordUsage(10_000_000, now) // 10000 credits at the pinned 1000/million rate
	status := window.Status(now)
	if !status.SelfLimited || status.Ceiling != 10000 {
		t.Fatalf("reserve units must cap the ceiling at 10000: %+v", status)
	}
	if !status.Capped {
		t.Fatal("10000 of a 10000 ceiling must be capped")
	}
}

func TestUsageCapReachesRouterAndPauseReason(t *testing.T) {
	a := chatServer(t, "a")
	b := chatServer(t, "b")
	first := testProvider("a", a.URL, 100)
	first.Quota.Local.Unit = "tokens"
	first.Quota.MaxUsageRatio = 0.5
	second := testProvider("b", b.URL, 100)
	second.Quota.Local.Unit = "tokens"
	second.Quota.MaxUsageRatio = 0.5
	pool, err := NewPool(testConfig(first, second))
	if err != nil {
		t.Fatal(err)
	}
	pool.chargeUsage("a", llm.UsageUpdate{RequestID: 1, TotalTokens: 60, Final: true})
	statuses := pool.Statuses()
	if !statuses[0].Capped {
		t.Fatalf("source a should be capped at 50%%: %+v", statuses[0])
	}
	if pool.ActiveID() != "a" {
		t.Fatalf("active should still be a until routing runs, got %q", pool.ActiveID())
	}
	// The capped source must not receive new work: the next selection goes to b.
	provider := pool.nextProvider(map[string]bool{})
	if provider == nil || provider.ID() != "b" {
		t.Fatalf("router ignored the usage cap, chose %v", provider)
	}
	// Cap both sources: the pause reason must say it is the self-imposed cap.
	pool.chargeUsage("b", llm.UsageUpdate{RequestID: 2, TotalTokens: 60, Final: true})
	budget := pool.EarliestReset()
	if budget.EarliestReset.IsZero() || !strings.Contains(budget.Reason, "自设用量上限") {
		t.Fatalf("pause reason = %q", budget.Reason)
	}
}

func TestUsageLimitPersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/limits.json"
	entry := testProvider("glm", "http://example.invalid", 100)
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetLimitsPath(path)
	if err := pool.SetUsageLimit("glm", 0.9, 0); err != nil {
		t.Fatal(err)
	}
	if ratio, _, ok := pool.UsageLimit("glm"); !ok || ratio != 0.9 {
		t.Fatalf("limit not applied: %v %v", ratio, ok)
	}
	reloaded, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetLimitsPath(path)
	if err := reloaded.LoadLimits(); err != nil {
		t.Fatal(err)
	}
	if ratio, _, _ := reloaded.UsageLimit("glm"); ratio != 0.9 {
		t.Fatalf("limit not restored: %v", ratio)
	}
	if err := reloaded.SetUsageLimit("nope", 0.5, 0); err == nil {
		t.Fatal("unknown provider must fail")
	}
	if err := reloaded.SetUsageLimit("glm", 2, 0); err == nil {
		t.Fatal("ratio above 1 must fail")
	}
	if err := reloaded.SetUsageLimit("glm", 1, -1); err == nil {
		t.Fatal("negative reserve must fail")
	}
}

// Two channels of the same vendor are independent accounts: charge one and the
// other must keep its own remaining quota.
func TestTwoChannelsOfSameVendorKeepSeparateQuota(t *testing.T) {
	first := testProvider("glm-a", "http://example.invalid", 100)
	first.Vendor, first.Plan = "glm", "glm-lite"
	first.Quota.Local.Unit = "tokens"
	first.Quota.MaxUsageRatio = 0.5
	second := testProvider("glm-b", "http://example.invalid", 1000)
	second.Vendor, second.Plan = "glm", "glm-max"
	second.Quota.Local.Unit = "tokens"
	second.Quota.MaxUsageRatio = 0.9

	pool, err := NewPool(testConfig(first, second))
	if err != nil {
		t.Fatal(err)
	}
	pool.chargeUsage("glm-a", llm.UsageUpdate{RequestID: 1, TotalTokens: 60, Final: true})

	statuses := pool.Statuses()
	a, b := statuses[0], statuses[1]
	if a.Plan != "glm-lite" || b.Plan != "glm-max" || a.Vendor != "glm" || b.Vendor != "glm" {
		t.Fatalf("channel identity lost: %+v %+v", a, b)
	}
	if !a.Capped {
		t.Fatalf("channel a should be capped at 50%% of 100: %+v", a)
	}
	if b.Used != 0 || b.Capped {
		t.Fatalf("channel b must be untouched: %+v", b)
	}
	if b.Ceiling != 900 {
		t.Fatalf("channel b ceiling should follow its own plan: %v", b.Ceiling)
	}
	if pool.ActiveID() != "glm-a" {
		t.Fatalf("active changed unexpectedly: %s", pool.ActiveID())
	}
	if next := pool.nextProvider(map[string]bool{}); next == nil || next.ID() != "glm-b" {
		t.Fatalf("router must fall through to the other channel")
	}
}

// Percent readings are folded into the channel's unit so the self-imposed cap
// keeps working in credits.
func TestConvertSampleFoldsPercentIntoChannelUnit(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 28000)
	entry.Quota.Local.Unit = "credits"
	entry.Quota.Probe.Unit = "percent"
	converted := convertSample(Sample{Used: 25, Total: 100, Unit: "percent", HasUsed: true, HasTotal: true}, entry)
	if converted.Used != 7000 || converted.Total != 28000 || converted.Unit != "credits" {
		t.Fatalf("percent not converted: %+v", converted)
	}

	// No configured capacity: keep the raw percentage scale.
	unknown := testProvider("mm", "http://example.invalid", 0)
	unknown.Quota.Probe.Unit = "percent"
	raw := convertSample(Sample{Used: 40, HasUsed: true}, unknown)
	if raw.Unit != "percent" || raw.Total != 100 || raw.Used != 40 {
		t.Fatalf("percent scale should stay raw without capacity: %+v", raw)
	}

	// A native-credit probe is untouched.
	native := testProvider("glm2", "http://example.invalid", 1000)
	plain := convertSample(Sample{Used: 300, Total: 1000, HasUsed: true, HasTotal: true}, native)
	if plain.Used != 300 || plain.Total != 1000 {
		t.Fatalf("native sample modified: %+v", plain)
	}
}

func TestPlanDiffers(t *testing.T) {
	cases := []struct {
		configured, observed string
		want                 bool
	}{
		{"glm-max", "max", false},
		{"glm-pro", "max", true},
		{"glm-max", "", false},
		{"", "max", false},
	}
	for _, test := range cases {
		if got := planDiffers(test.configured, test.observed); got != test.want {
			t.Fatalf("%q vs %q = %v want %v", test.configured, test.observed, got, test.want)
		}
	}
}

// Fallback metering must use the cheap model's coefficients: the previous 5.3
// rate overstated consumption by ~3x.
func TestFallbackChargeUsesFlashRate(t *testing.T) {
	local := config.LocalQuotaConfig{Unit: "credits"} // rate unset → default
	if got := chargeFor(local, 1_000_000, time.Now()); math.Abs(got-515) > 1e-6 {
		t.Fatalf("default rate = %v, want 515 (GLM-5.3-Flash)", got)
	}
	glm := config.LocalQuotaConfig{Unit: "credits", CreditsPerMillionTokens: 515}
	if got := chargeFor(glm, 500_000, time.Now()); math.Abs(got-257.5) > 1e-6 {
		t.Fatalf("flash charge = %v", got)
	}
}

// A source quarantined before it ever recorded usage must still roll over.
// Regression: advanceLocked used to require a non-zero window start, so a single
// billing error blocked the channel for the whole 5h window and, once persisted,
// for every later run as well.
func TestQuarantineExpiresWithoutPriorUsage(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"code":"402","message":"insufficient balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"recovered"}}]}`))
	}))
	defer server.Close()

	pool, err := NewPool(testConfig(testProvider("glm", server.URL, 0)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	pool.now = func() time.Time { return now }

	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); !errors.Is(err, ErrAllExhausted) {
		t.Fatalf("want ErrAllExhausted, got %v", err)
	}
	failing.Store(false)

	// A billing error without a retry hint must not lock the channel for 5h.
	now = now.Add(6 * time.Minute)
	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("source did not recover after a bounded cooldown: %v", err)
	}

	// And even a full window quarantine must roll over.
	failing.Store(true)
	provider := pool.providers[0]
	provider.quota.markExhausted(now, now.Add(5*time.Hour), "额度耗尽")
	failing.Store(false)
	now = now.Add(5*time.Hour + time.Minute)
	if !provider.quota.Ready(now) {
		t.Fatal("window did not roll over after its reset instant")
	}
}

// A persisted exhausted flag with no reset instant must not survive a restart.
func TestRestoreClearsStaleExhaustion(t *testing.T) {
	provider := testProvider("glm", "http://example.invalid", 100)
	window, err := newQuotaWindow(provider)
	if err != nil {
		t.Fatal(err)
	}
	window.restore(quotaState{Provider: "glm", Exhausted: true, Used: 10, Total: 100}, time.Now())
	if !window.Ready(time.Now()) {
		t.Fatal("exhaustion without a reset instant must be dropped on restore")
	}
}

func TestExhaustionReportNamesEveryChannel(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 100)
	entry.Quota.Local.Unit = "tokens"
	entry.Quota.MaxUsageRatio = 0.5
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pool.now = func() time.Time { return now }
	pool.chargeUsage("glm", llm.UsageUpdate{RequestID: 1, TotalTokens: 80, Final: true})
	report := pool.ExhaustionReport()
	if !strings.Contains(report, "glm") || !strings.Contains(report, "自设上限") {
		t.Fatalf("report must explain why the channel is unusable: %q", report)
	}
}

// A billing error inside an already-open window must not block the channel until
// the window resets: only the short cooldown applies.
func TestBillingErrorDoesNotDrainAnOpenWindow(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"code":"402","message":"insufficient balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	entry := testProvider("glm", server.URL, 100)
	entry.Quota.Local.Unit = "tokens"
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	pool.now = func() time.Time { return now }
	pool.chargeUsage("glm", llm.UsageUpdate{RequestID: 99, TotalTokens: 40, Final: true})
	if status := pool.Statuses()[0]; status.ResetAt.IsZero() || status.Used == 0 {
		t.Fatalf("window should be open with usage: %+v", status)
	}

	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err == nil {
		t.Fatal("402 must surface")
	}
	status := pool.Statuses()[0]
	if status.Exhausted {
		t.Fatalf("an unconfirmed 402 must not mark the window drained: %+v", status)
	}
	if !status.CooldownTill.After(now) {
		t.Fatal("a cooldown should be applied")
	}

	failing.Store(false)
	now = now.Add(quarantineCooldown + time.Minute)
	if _, err := pool.Chat(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err != nil {
		t.Fatalf("channel should recover after the cooldown: %v", err)
	}
	if status := pool.Statuses()[0]; status.Used < 40 {
		t.Fatalf("recovered usage should keep the earlier consumption: %+v", status)
	}
}

// The pause reason must describe the real brake: a short cooldown is not a
// drained 5h window.
func TestPauseReasonMatchesTheActualBrake(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 100)
	entry.Quota.Local.Unit = "tokens"
	entry.Quota.MaxUsageRatio = 0.5
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pool.now = func() time.Time { return now }

	// Cooldown only.
	pool.providers[0].quota.park(now, now.Add(90*time.Second), "429")
	budget := pool.EarliestReset()
	if !strings.Contains(budget.Reason, "冷却") || strings.Contains(budget.Reason, "5 小时余量") {
		t.Fatalf("cooldown reported as a drained window: %q", budget.Reason)
	}
	if budget.Detail == "" {
		t.Fatal("detail must name the channel state")
	}

	// Self-imposed cap.
	pool.providers[0].quota = windowForTotal(t, 100, "tokens", 0.5)
	pool.chargeUsage("glm", llm.UsageUpdate{RequestID: 1, TotalTokens: 80, Final: true})
	budget = pool.EarliestReset()
	if !strings.Contains(budget.Reason, "自设用量上限") {
		t.Fatalf("cap reason missing: %q", budget.Reason)
	}

	// Vendor-confirmed exhaustion.
	pool.providers[0].quota = windowForTotal(t, 100, "tokens", 1)
	pool.chargeUsage("glm", llm.UsageUpdate{RequestID: 2, TotalTokens: 100, Final: true})
	budget = pool.EarliestReset()
	if !strings.Contains(budget.Reason, "余量") {
		t.Fatalf("exhaustion reason missing: %q", budget.Reason)
	}
}

func windowForTotal(t *testing.T, total float64, unit string, cap float64) *QuotaWindow {
	t.Helper()
	entry := testProvider("glm", "http://example.invalid", total)
	entry.Quota.Local.Unit = unit
	entry.Quota.MaxUsageRatio = cap
	window, err := newQuotaWindow(entry)
	if err != nil {
		t.Fatal(err)
	}
	return window
}
