package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

// QuotaWindow is the local model of one vendor's rolling subscription window.
//
// Both GLM Coding Plan and MiniMax M Plan refresh on a 5 hour cycle that starts
// at first use, and unused quota does not carry over. The window therefore needs
// three states: idle (start zero), running (start + resetAt set) and drained
// (exhausted/cooldown), and it must survive a process restart so a restart does
// not look like a fresh window.
type QuotaWindow struct {
	mu       sync.RWMutex
	provider string
	cfg      config.QuotaConfig
	window   time.Duration
	unit     string

	used      float64
	total     float64
	start     time.Time
	resetAt   time.Time
	cooldown  time.Time
	exhausted bool
	source    string
	updatedAt time.Time
	reason    string
	// observedPlan is what the vendor reports (e.g. data.level=pro), used to
	// catch a plan mismatch without trusting the config blindly.
	observedPlan string
}

type quotaState struct {
	Version    int       `json:"version"`
	Provider   string    `json:"provider"`
	Used       float64   `json:"used"`
	Total      float64   `json:"total"`
	Unit       string    `json:"unit"`
	Start      time.Time `json:"start"`
	ResetAt    time.Time `json:"reset_at"`
	Cooldown   time.Time `json:"cooldown_till"`
	Exhausted  bool      `json:"exhausted"`
	Source     string    `json:"source"`
	Reason     string    `json:"reason,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	WindowSecs float64   `json:"window_seconds"`
}

func newQuotaWindow(cfg config.ProviderConfig) (*QuotaWindow, error) {
	window, err := cfg.Quota.WindowDuration()
	if err != nil {
		return nil, err
	}
	unit := cfg.Quota.Local.Unit
	if unit == "" {
		unit = "credits"
	}
	return &QuotaWindow{
		provider: cfg.ID,
		cfg:      cfg.Quota,
		window:   window,
		unit:     unit,
		total:    cfg.Quota.Local.Total,
		source:   "local",
	}, nil
}

// advanceLocked rolls the window over once its reset instant has passed. An
// expired window returns to idle instead of starting a new one, because the
// vendor only starts the next window on the first request.
func (q *QuotaWindow) advanceLocked(now time.Time) {
	// resetAt alone is enough: a channel can be quarantined before it ever
	// recorded usage (start stays zero), and that must still expire.
	if !q.resetAt.IsZero() && !now.Before(q.resetAt) {
		q.used = 0
		q.start = time.Time{}
		q.resetAt = time.Time{}
		q.exhausted = false
		q.cooldown = time.Time{}
		q.reason = ""
		q.updatedAt = now
	}
}

func (q *QuotaWindow) windowSeconds() time.Duration {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.window
}

// ceilingLocked is the usable portion of the window after the user's
// self-imposed cap. A cap of 90% means this program stops at 90% and leaves the
// rest for other tools sharing the subscription.
func (q *QuotaWindow) ceilingLocked() (float64, bool) {
	if q.total <= 0 {
		return 0, false
	}
	ceiling := q.total
	if q.cfg.ReserveUnits > 0 && q.cfg.ReserveUnits < ceiling {
		ceiling = q.total - q.cfg.ReserveUnits
	}
	if q.cfg.MaxUsageRatio > 0 && q.cfg.MaxUsageRatio < 1 {
		if capped := q.total * q.cfg.MaxUsageRatio; capped < ceiling {
			ceiling = capped
		}
	}
	if ceiling < 0 {
		ceiling = 0
	}
	return ceiling, true
}

// Capped reports that the user's own ceiling, not the vendor, stopped this
// source. It is what turns "余量不足" into "已达自设上限" in the UI.
func (q *QuotaWindow) Capped(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.advanceLocked(now)
	ceiling, ok := q.ceilingLocked()
	return ok && q.used >= ceiling && ceiling < q.total
}

// Ratio is the fraction of the *usable* window still available. Unknown totals
// report 1: an unmeasured source must never be treated as empty.
func (q *QuotaWindow) Ratio(now time.Time) float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.advanceLocked(now)
	if q.exhausted || now.Before(q.cooldown) {
		return 0
	}
	ceiling, ok := q.ceilingLocked()
	if !ok {
		return 1
	}
	if ceiling <= 0 {
		return 0
	}
	remaining := ceiling - q.used
	if remaining <= 0 {
		return 0
	}
	return math.Min(1, remaining/ceiling)
}

// Ready reports whether the source may receive a new request right now.
func (q *QuotaWindow) Ready(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.advanceLocked(now)
	if q.exhausted || now.Before(q.cooldown) {
		return false
	}
	ceiling, ok := q.ceilingLocked()
	if !ok {
		return true
	}
	if ceiling <= 0 {
		return false
	}
	remaining := ceiling - q.used
	return remaining > 0 && remaining/ceiling >= q.cfg.MinHeadroom
}

func (q *QuotaWindow) Status(now time.Time) Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.advanceLocked(now)
	status := Status{Used: q.used, Total: q.total, Unit: q.unit, ResetAt: q.resetAt,
		CooldownTill: q.cooldown, Exhausted: q.exhausted, Source: q.source, UpdatedAt: q.updatedAt}
	status.MaxUsageRatio = q.cfg.MaxUsageRatio
	status.PlanObserved = q.observedPlan
	if ceiling, ok := q.ceilingLocked(); ok {
		status.Ceiling = ceiling
		status.SelfLimited = ceiling < q.total
		status.Capped = q.used >= ceiling
		status.Remaining = math.Max(0, q.total-q.used)
		status.Usable = math.Max(0, ceiling-q.used)
		if ceiling > 0 {
			status.Ratio = math.Min(1, status.Usable/ceiling)
		}
	} else {
		status.Ratio = 1
	}
	return status
}

func (q *QuotaWindow) resetAtTime() time.Time {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.resetAt
}

// RecordUsage charges one completed request to the window. Tokens are converted
// to the local unit using the vendor's published coefficients.
func (q *QuotaWindow) RecordUsage(tokens int64, now time.Time) {
	if tokens <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.advanceLocked(now)
	if q.start.IsZero() {
		q.start = now
		q.resetAt = now.Add(q.window)
	}
	q.used += chargeFor(q.cfg.Local, tokens, now)
	q.updatedAt = now
	if q.total > 0 && q.used >= q.total {
		q.exhausted = true
		q.reason = "本地计量显示额度已耗尽"
	}
}

// Limits reports the live cap (runtime overrides included).
func (q *QuotaWindow) Limits() (ratio, reserve float64) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.cfg.MaxUsageRatio, q.cfg.ReserveUnits
}

// SetUsageLimit changes the self-imposed cap at runtime (UI: /limit).
func (q *QuotaWindow) SetUsageLimit(ratio, reserve float64) error {
	if ratio < 0 || ratio > 1 {
		return fmt.Errorf("用量上限必须在 (0,1] 之间")
	}
	if reserve < 0 {
		return fmt.Errorf("保留量必须非负")
	}
	if ratio == 0 {
		ratio = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cfg.MaxUsageRatio = ratio
	q.cfg.ReserveUnits = reserve
	q.updatedAt = time.Now()
	return nil
}

// observePlan records the vendor-reported tier.
func (q *QuotaWindow) observePlan(plan string) {
	if plan == "" {
		return
	}
	q.mu.Lock()
	q.observedPlan = plan
	q.mu.Unlock()
}

// quarantineCooldown is how long an unconfirmed quota error parks a channel.
// A single 402 may be a gateway or billing hiccup, so it must not lock the
// channel for a whole window.
const quarantineCooldown = 5 * time.Minute

// park applies a short cooldown without declaring the window empty. Only a
// vendor reading (probe) or a vendor error carrying a reset hint may mark a
// window exhausted.
func (q *QuotaWindow) park(now time.Time, until time.Time, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.updatedAt = now
	q.reason = reason
	if until.After(now) && until.After(q.cooldown) {
		q.cooldown = until
	}
}

func (q *QuotaWindow) markExhausted(now time.Time, until time.Time, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.exhausted = true
	q.reason = reason
	q.updatedAt = now
	if until.After(now) {
		q.cooldown = until
		if q.resetAt.IsZero() || q.resetAt.Before(until) {
			q.resetAt = until
		}
	}
}

func (q *QuotaWindow) markRateLimited(now time.Time, delay time.Duration, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.updatedAt = now
	if delay <= 0 {
		delay = 30 * time.Second
	}
	until := now.Add(delay)
	if until.After(q.cooldown) {
		q.cooldown = until
	}
	q.reason = reason
	if q.resetAt.IsZero() || q.resetAt.Before(until) {
		q.resetAt = until
	}
}

// applyProbe overwrites local estimates with an authoritative vendor reading.
func (q *QuotaWindow) applyProbe(sample Sample, now time.Time, unit string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if unit != "" {
		q.unit = unit
	}
	if sample.HasUsed {
		q.used = sample.Used
	}
	if sample.HasTotal {
		q.total = sample.Total
	}
	if sample.HasReset && sample.ResetAt.After(now) {
		q.resetAt = sample.ResetAt
		if q.start.IsZero() {
			q.start = sample.ResetAt.Add(-q.window)
		}
	}
	if sample.HasUsed || sample.HasTotal {
		q.exhausted = q.total > 0 && q.used >= q.total
		if !q.exhausted && !now.Before(q.cooldown) {
			q.cooldown = time.Time{}
			q.reason = ""
		}
	}
	q.source = "probe"
	q.updatedAt = now
}

func (q *QuotaWindow) snapshot() quotaState {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return quotaState{Version: 1, Provider: q.provider, Used: q.used, Total: q.total, Unit: q.unit,
		Start: q.start, ResetAt: q.resetAt, Cooldown: q.cooldown, Exhausted: q.exhausted,
		Source: q.source, Reason: q.reason, UpdatedAt: q.updatedAt, WindowSecs: q.window.Seconds()}
}

func (q *QuotaWindow) restore(state quotaState, now time.Time) {
	if state.Provider != "" && state.Provider != q.provider {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// Consumption is restored; capacity and unit are NOT. Configuration (or the
	// plan preset) is authoritative for what the window can hold, otherwise a
	// stale state file would keep an old plan's numbers after a plan change.
	q.used = state.Used
	q.start, q.resetAt, q.cooldown = state.Start, state.ResetAt, state.Cooldown
	q.exhausted, q.source, q.reason = state.Exhausted, state.Source, state.Reason
	if q.exhausted && !q.resetAt.After(now) {
		// A persisted "exhausted" without a future reset instant is stale state;
		// keeping it would block the channel forever.
		q.exhausted, q.reason = false, ""
	}
	q.updatedAt = state.UpdatedAt
	q.advanceLocked(now)
}

// chargeFor converts observed request tokens into the configured unit using the
// vendor's official coefficients and the off-peak discount.
func chargeFor(l config.LocalQuotaConfig, tokens int64, at time.Time) float64 {
	if tokens <= 0 {
		return 0
	}
	switch l.Unit {
	case "tokens":
		return float64(tokens)
	case "percent":
		// Percent plans give no token-to-quota exchange rate. Without an explicit
		// rate the local meter stays at zero and the vendor probe/429 signal owns
		// the decision, which is safer than inventing a conversion.
		if l.CreditsPerMillionTokens <= 0 {
			return 0
		}
	}
	rate := l.CreditsPerMillionTokens
	if rate <= 0 {
		// Default is GLM-5.3-Flash: 2.3 input / 0.56 cached / 8 output per 10k
		// tokens, blended to 515 per million. Metering on the expensive 5.3 rate
		// would overstate usage ~3x and trigger the cap far too early.
		rate = 515
	}
	return float64(tokens) / 1e6 * rate * l.Factor(at)
}

// ---------------------------------------------------------------------------
// Pool-level quota plumbing
// ---------------------------------------------------------------------------

// QuotaBudget describes how much time remains before a paused run may resume.
type QuotaBudget struct {
	EarliestReset time.Time
	Reason        string
	// Detail explains each channel's individual brake (cooldown, cap, window).
	Detail string
}

// EarliestReset is the soonest moment any enabled source could become usable
// again. It is zero when nothing is paused.
func (p *Pool) EarliestReset() QuotaBudget {
	now := p.now()
	p.mu.RLock()
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	var earliest time.Time
	cappedOnly, exhaustedAny, cooling := true, false, false
	considered := 0
	for _, provider := range providers {
		if !provider.Enabled() {
			continue
		}
		considered++
		if provider.quota.Ready(now) {
			return QuotaBudget{}
		}
		status := provider.quota.Status(now)
		if status.Exhausted {
			exhaustedAny = true
			cappedOnly = false
		} else if status.Capped {
			exhaustedAny = false
		} else {
			cappedOnly = false
			cooling = true
		}
		candidate := status.ResetAt
		if status.CooldownTill.After(candidate) {
			candidate = status.CooldownTill
		}
		if candidate.IsZero() {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	if considered == 0 {
		return QuotaBudget{}
	}
	if earliest.IsZero() {
		earliest = now.Add(5 * time.Minute)
	}
	// The reason must match reality: a 5 minute cooldown is not a drained 5 hour
	// window, and saying so sends people looking for a quota problem.
	reason := "所有启用源的 5 小时余量均已耗尽"
	switch {
	case cappedOnly:
		reason = "所有启用源均已达到自设用量上限（保留余量给其它工具），窗口刷新后自动继续"
	case !exhaustedAny && cooling:
		reason = "所有启用源均在限流冷却中，稍后自动重试"
	}
	return QuotaBudget{EarliestReset: earliest, Reason: reason, Detail: p.ExhaustionReport()}
}

// SetUsageLimit changes one source's self-imposed cap and persists it, so a
// restart keeps the user's "only use 90%" policy.
func (p *Pool) SetUsageLimit(id string, ratio, reserve float64) error {
	provider := p.find(id)
	if provider == nil {
		return fmt.Errorf("unknown provider %q", id)
	}
	if err := provider.quota.SetUsageLimit(ratio, reserve); err != nil {
		return err
	}
	return p.SaveLimits()
}

// UsageLimit reports the live cap of one source.
func (p *Pool) UsageLimit(id string) (ratio, reserve float64, ok bool) {
	provider := p.find(id)
	if provider == nil {
		return 0, 0, false
	}
	ratio, reserve = provider.quota.Limits()
	return ratio, reserve, true
}

type limitEntry struct {
	Provider string  `json:"provider"`
	Ratio    float64 `json:"max_usage_ratio"`
	Reserve  float64 `json:"reserve_units"`
}

type limitsFile struct {
	Version int          `json:"version"`
	SavedAt time.Time    `json:"saved_at"`
	Limits  []limitEntry `json:"limits"`
}

// SaveLimits writes runtime caps next to the quota window state.
func (p *Pool) SaveLimits() error {
	p.mu.RLock()
	path := p.limitsPath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	file := limitsFile{Version: 1, SavedAt: p.now()}
	for _, provider := range providers {
		ratio := provider.quota.cfg.MaxUsageRatio
		reserve := provider.quota.cfg.ReserveUnits
		if ratio >= 1 && reserve == 0 {
			continue
		}
		file.Limits = append(file.Limits, limitEntry{Provider: provider.ID(), Ratio: ratio, Reserve: reserve})
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadLimits applies persisted runtime caps over the configuration.
func (p *Pool) LoadLimits() error {
	p.mu.RLock()
	path := p.limitsPath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var file limitsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析用量上限状态失败：%w", err)
	}
	if file.Version != 1 {
		return fmt.Errorf("不支持的用量上限状态版本 %d", file.Version)
	}
	for _, entry := range file.Limits {
		for _, provider := range providers {
			if provider.ID() != entry.Provider {
				continue
			}
			if err := provider.quota.SetUsageLimit(entry.Ratio, entry.Reserve); err != nil {
				return fmt.Errorf("源 %s: %w", entry.Provider, err)
			}
		}
	}
	return nil
}

// SetLimitsPath configures where runtime caps are persisted.
func (p *Pool) SetLimitsPath(path string) {
	p.mu.Lock()
	p.limitsPath = path
	p.mu.Unlock()
}

// QuotaStatePathHolder keeps the persistence location configurable per pool.
func (p *Pool) SetQuotaStatePath(path string) {
	p.mu.Lock()
	p.quotaStatePath = path
	p.mu.Unlock()
}

// SaveQuotaState writes the window snapshot atomically. Restarting the program
// must not reset a half-consumed 5 hour window.
func (p *Pool) SaveQuotaState() error {
	p.mu.RLock()
	path := p.quotaStatePath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	state := persistedQuota{Version: 1, SavedAt: p.now()}
	for _, provider := range providers {
		state.Providers = append(state.Providers, provider.quota.snapshot())
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".quota-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

type persistedQuota struct {
	Version   int          `json:"version"`
	SavedAt   time.Time    `json:"saved_at"`
	Providers []quotaState `json:"providers"`
}

// LoadQuotaState restores windows saved by a previous process.
func (p *Pool) LoadQuotaState() error {
	p.mu.RLock()
	path := p.quotaStatePath
	providers := append([]*Provider(nil), p.providers...)
	p.mu.RUnlock()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var state persistedQuota
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("解析余量状态失败：%w", err)
	}
	if state.Version != 1 {
		return fmt.Errorf("不支持的余量状态版本 %d", state.Version)
	}
	now := p.now()
	for _, saved := range state.Providers {
		for _, provider := range providers {
			if provider.ID() == saved.Provider {
				provider.quota.restore(saved, now)
			}
		}
	}
	return nil
}

// chargeUsage attributes one finished request to the provider that served it.
// Only final updates carry an authoritative total, so intermediate estimates are
// ignored; this entry point exists for direct accounting (UI, tests).
func (p *Pool) chargeUsage(providerID string, update llm.UsageUpdate) {
	if update.TotalTokens <= 0 || !update.Final {
		return
	}
	provider := p.find(providerID)
	if provider == nil {
		return
	}
	provider.quota.RecordUsage(update.TotalTokens, p.now())
}

// attemptMeter accumulates a single logical request's usage so it can be
// committed only if the request actually succeeded.
type attemptMeter struct {
	mu    sync.Mutex
	seen  map[uint64]int64
	total int64
}

func (m *attemptMeter) Add(update llm.UsageUpdate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if update.TotalTokens < 0 {
		return
	}
	if m.seen == nil {
		m.seen = map[uint64]int64{}
	}
	previous := m.seen[update.RequestID]
	// Provider usage can correct the initial estimate downward. Keep the latest
	// cumulative value for each request instead of its high-water mark.
	m.total += update.TotalTokens - previous
	m.seen[update.RequestID] = update.TotalTokens
}

func (m *attemptMeter) Take() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := m.total
	m.total, m.seen = 0, nil
	return total
}

// observe accumulates usage for one attempt. commit reports whether the attempt
// succeeded; failed requests are not charged, otherwise a rejected call would
// burn quota and even open a new window (start/resetAt written on error).
func (p *Pool) observe(ctx context.Context, providerID string) (context.Context, func(bool)) {
	meter := &attemptMeter{}
	observed := llm.ChainUsageObserver(ctx, meter.Add)
	return observed, func(commit bool) {
		tokens := meter.Take()
		if !commit || tokens <= 0 {
			return
		}
		if provider := p.find(providerID); provider != nil {
			provider.quota.RecordUsage(tokens, p.now())
		}
	}
}
