// Package provider owns the multi-source LLM pool: registration, health,
// quota-aware routing and failover. It deliberately implements llm.Client and
// llm.ToolClient so the agent layer needs no knowledge of how many vendors are
// behind a request.
package provider

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/applog"
	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

// Provider is one LLM source. Its client is immutable once built; only the
// enabled flag, quota window and cooldown are mutable and all are guarded.
type Provider struct {
	cfg    config.ProviderConfig
	client *llm.OpenAIClient
	// aux is the compression lane: same vendor endpoint and key, but the
	// compressor model and a non-streaming request shape.
	aux     *llm.OpenAIClient
	quota   *QuotaWindow
	mu      sync.RWMutex
	enabled bool
	// lastProbeUnix is read/written atomically by the quota loop.
	lastProbeUnix int64
	// disabledReason explains why a channel is out of rotation, so "why didn't
	// it fail over?" has an answer in the error text itself.
	disabledReason string
	limiter        *Limiter
}

func (p *Provider) ID() string    { return p.cfg.ID }
func (p *Provider) Label() string { return p.cfg.Label }

func (p *Provider) Enabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.enabled
}

func (p *Provider) setEnabled(value bool) {
	p.mu.Lock()
	p.enabled = value
	if value {
		p.disabledReason = ""
	}
	p.mu.Unlock()
}

func (p *Provider) setDisabledReason(reason string) {
	p.mu.Lock()
	p.disabledReason = reason
	p.mu.Unlock()
}

// DisabledReason reports why the channel is out of rotation ("" when enabled).
func (p *Provider) DisabledReason() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.enabled {
		return ""
	}
	if p.disabledReason == "" {
		return "已停用"
	}
	return p.disabledReason
}

// Model reports the model name actually used, for status display and tests.
func (p *Provider) Model() string { return p.cfg.OpenAI.Model }

// Status is a copy of everything the UI needs; never hand out live pointers.
type Status struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Vendor string `json:"vendor"`
	// Plan is the subscription tier of this channel; capacity comes from it.
	Plan         string    `json:"plan"`
	Model        string    `json:"model"`
	Enabled      bool      `json:"enabled"`
	Active       bool      `json:"active"`
	Weight       int       `json:"weight"`
	Used         float64   `json:"used"`
	Total        float64   `json:"total"`
	Remaining    float64   `json:"remaining"`
	Ratio        float64   `json:"ratio"`
	Unit         string    `json:"unit"`
	ResetAt      time.Time `json:"reset_at"`
	CooldownTill time.Time `json:"cooldown_till"`
	Exhausted    bool      `json:"exhausted"`
	Source       string    `json:"source"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastError    string    `json:"last_error,omitempty"`
	// Self-imposed usage cap: Ceiling is the portion this program may consume,
	// Usable is what is left of it, Capped means the cap (not the vendor) stops us.
	// Scheduled reports whether the channel's working-hours policy allows it now.
	Scheduled bool `json:"scheduled"`
	// DisabledReason explains an out-of-rotation channel ("" when enabled).
	DisabledReason string  `json:"disabled_reason,omitempty"`
	Ceiling        float64 `json:"ceiling"`
	Usable         float64 `json:"usable"`
	SelfLimited    bool    `json:"self_limited"`
	Capped         bool    `json:"capped"`
	MaxUsageRatio  float64 `json:"max_usage_ratio"`
	// PlanObserved is the tier the vendor reports for this channel.
	PlanObserved string `json:"plan_observed,omitempty"`
	// Adaptive concurrency: the limit currently in force, its ceiling and how
	// many 429s this source has absorbed.
	Concurrency int `json:"concurrency"`
	// Learned is the stable value remembered from previous runs (0 = none).
	Learned   int `json:"learned_concurrency"`
	MaxConcur int `json:"max_concurrency"`
	InFlight  int `json:"in_flight"`
	Throttles int `json:"throttles"`
}

// Pool routes every model request to one provider and fails over on quota
// exhaustion. A nil *Pool is not valid; callers always build one via NewPool.
type Pool struct {
	mu          sync.RWMutex
	providers   []*Provider
	active      string
	activeSince time.Time
	routing     config.RoutingConfig
	now         func() time.Time
	// onSwitch reports a real provider change so the UI and logs can explain it.
	onSwitch func(from, to, reason string)
	// onConcurrency reports adaptive concurrency changes.
	onConcurrency func(providerID string, from, to int, reason string)
	// waiter is the 429 backoff primitive, replaceable in tests.
	waiter func(ctx context.Context, delay time.Duration) error
	// clock is the per-channel working-hours policy. It lives in the scheduler,
	// so the pool only sees a tiny interface and no import cycle appears.
	clock ChannelClock
	// notes is a bounded ring of routing/throttling notices for the UI.
	notes []string
	// lastErr keeps the most recent provider error per source for /providers.
	lastErr map[string]string
	// lineage maps a Responses API id to the provider that produced it. Vendor
	// continuation ids are not portable, so the pool strips them on a switch.
	lineage map[string]string
	// quotaStatePath is where window state persists across restarts.
	quotaStatePath string
	// limitsPath persists runtime /limit changes.
	limitsPath string
	// concurrencyPath persists learned, stable concurrency per channel.
	concurrencyPath string
	// selectionPath persists the user's /mode choice.
	selectionPath string
	// modelOverrides remembers runtime model changes per channel.
	modelOverrides map[string]string
	// configuredModels is the model from config, restored by ClearSelection.
	configuredModels map[string]string
	// compressCfg is kept so a rebuilt client gets the same compression lane.
	compressCfg config.OpenAIConfig
}

// NewPool builds one client per configured source. Provider-specific settings
// survive because each source keeps its own OpenAIConfig.
func NewPool(cfg config.Config) (*Pool, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	// Normalise concurrency even when the caller built Config directly instead of
	// loading a validated file: a zero ceiling would freeze the limiter.
	cfg.Concurrency = cfg.Concurrency.WithDefaults(cfg.Agent.ReconAgents + cfg.Agent.AuditAgents + 1)
	pool := &Pool{routing: cfg.Routing, now: time.Now, lastErr: map[string]string{}, lineage: map[string]string{},
		compressCfg: cfg.CompressOpenAI, modelOverrides: map[string]string{}, configuredModels: map[string]string{}}
	if pool.routing.SwitchBelow == 0 {
		pool.routing.SwitchBelow = 0.15
	}
	if pool.routing.MinDwellSeconds == 0 {
		pool.routing.MinDwellSeconds = 300
	}
	for _, entry := range cfg.Providers {
		window, err := newQuotaWindow(entry)
		if err != nil {
			return nil, err
		}
		pool.configuredModels[entry.ID] = entry.OpenAI.Model
		provider := &Provider{cfg: entry, client: llm.NewOpenAIClient(entry.OpenAI), aux: auxClient(cfg.CompressOpenAI, entry.OpenAI),
			quota: window, enabled: entry.IsEnabled(), limiter: newLimiter(cfg.Concurrency)}
		// Learn once, reuse on the next start: re-probing concurrency from 4 every
		// launch means re-triggering 429s that were already solved.
		id := entry.ID
		provider.limiter.OnSettle(func(limit int) { pool.noteSettle(id, limit) })
		pool.providers = append(pool.providers, provider)
	}
	// A source without a key cannot serve anything. Skip it instead of failing
	// the first request, but only when another source is usable: a completely
	// keyless config must still start (the classic "fill the key later" flow).
	if err := pool.skipKeyless(); err != nil {
		return nil, err
	}
	if err := pool.pickInitial(); err != nil {
		return nil, err
	}
	return pool, nil
}

// skipKeyless disables sources with a missing API key and records why.
func (p *Pool) skipKeyless() error {
	usable := 0
	for _, provider := range p.providers {
		if strings.TrimSpace(provider.cfg.OpenAI.APIKey) != "" {
			usable++
		}
	}
	if usable == 0 {
		return nil
	}
	for _, provider := range p.providers {
		if !provider.Enabled() || strings.TrimSpace(provider.cfg.OpenAI.APIKey) != "" {
			continue
		}
		reason := fmt.Sprintf("未配置 API key（%s）", envHint(provider.cfg.OpenAI))
		provider.setEnabled(false)
		provider.setDisabledReason(reason)
		p.appendNote(fmt.Sprintf("源 %s %s，已跳过；填入后重启或 /providers 启用", provider.ID(), reason))
	}
	return nil
}

func envHint(cfg config.OpenAIConfig) string {
	if cfg.APIKeyEnv != "" {
		return cfg.APIKeyEnv
	}
	return "openai.api_key"
}

// auxClient derives the compression client for one source. Compression must not
// become the single point of quota failure, so it rides the same pool.
// rebuildClient recreates a channel's client after its model changed.
func rebuildClient(provider config.ProviderConfig, compress config.OpenAIConfig) *llm.OpenAIClient {
	return llm.NewOpenAIClient(provider.OpenAI)
}

// sameEndpoint reports whether two base URLs point at the same vendor host. The
// compression settings are derived from the *tightest* provider, so borrowing its
// model name on a different vendor produces "model_not_found".
func sameEndpoint(a, b string) bool {
	host := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return ""
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return raw
		}
		return strings.ToLower(parsed.Host)
	}
	left, right := host(a), host(b)
	if left == "" || right == "" {
		return true
	}
	return left == right
}

func auxClient(compress config.OpenAIConfig, source config.OpenAIConfig) *llm.OpenAIClient {
	derived := source
	// Only borrow the compression model when it belongs to the same endpoint;
	// otherwise keep the channel's own model and just take the size/effort knobs.
	if compress.Model != "" && sameEndpoint(compress.BaseURL, source.BaseURL) {
		derived.Model = compress.Model
	}
	if compress.MaxContextTokens > 0 {
		derived.MaxContextTokens = compress.MaxContextTokens
	}
	if compress.MaxOutputTokens > 0 {
		derived.MaxOutputTokens = compress.MaxOutputTokens
	}
	if compress.ReasoningEffort != "" {
		derived.ReasoningEffort = compress.ReasoningEffort
	}
	if compress.Temperature != 0 {
		derived.Temperature = compress.Temperature
	}
	derived.Stream = false
	return llm.NewOpenAIClient(derived)
}

func (p *Pool) pickInitial() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	candidates := p.orderLocked(nil)
	if len(candidates) == 0 {
		return fmt.Errorf("no provider is enabled")
	}
	p.active = candidates[0].ID()
	p.activeSince = p.now()
	return nil
}

// orderLocked returns enabled providers in preference order. excluded holds ids
// already tried for this request.
func (p *Pool) orderLocked(excluded map[string]bool) []*Provider {
	var out []*Provider
	for _, provider := range p.providers {
		if !provider.Enabled() || excluded[provider.ID()] {
			continue
		}
		out = append(out, provider)
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].quota.Ratio(p.now()), out[j].quota.Ratio(p.now())
		if li != lj {
			return li > lj
		}
		if out[i].cfg.Weight != out[j].cfg.Weight {
			return out[i].cfg.Weight > out[j].cfg.Weight
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

func (p *Pool) find(id string) *Provider {
	for _, provider := range p.providers {
		if provider.ID() == id {
			return provider
		}
	}
	return nil
}

func (p *Pool) byID(id string) (*Provider, error) {
	if provider := p.find(id); provider != nil {
		return provider, nil
	}
	return nil, fmt.Errorf("unknown provider %q", id)
}

// ActiveID returns the provider currently receiving requests.
func (p *Pool) ActiveID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.active
}

// OnConcurrency reports adaptive concurrency changes (429 shrink, success grow).
func (p *Pool) OnConcurrency(fn func(providerID string, from, to int, reason string)) {
	p.mu.Lock()
	p.onConcurrency = fn
	p.mu.Unlock()
}

func (p *Pool) notifyConcurrency(providerID string, from, to int, reason string) {
	p.appendNote(fmt.Sprintf("%s 并发 %d → %d（%s）", providerID, from, to, reason))
	p.mu.RLock()
	fn := p.onConcurrency
	p.mu.RUnlock()
	if fn != nil && from != to {
		fn(providerID, from, to, reason)
	}
}

// ChannelClock answers whether a given channel may run at a given instant.
// The scheduler implements it as "channel override, else global window".
type ChannelClock interface {
	OpenAt(channel string, at time.Time) bool
}

// SetChannelClock installs the per-channel working-hours policy.
func (p *Pool) SetChannelClock(clock ChannelClock) {
	p.mu.Lock()
	p.clock = clock
	p.mu.Unlock()
}

// channelOpen reports whether routing may use this channel right now.
func (p *Pool) channelOpen(id string, at time.Time) bool {
	p.mu.RLock()
	clock := p.clock
	p.mu.RUnlock()
	return clockOpenAt(clock, id, at)
}

// clockOpenAt evaluates a clock without touching the pool lock. Statuses() and
// other read paths already hold it, and RLock while holding RLock can deadlock
// as soon as a writer queues.
func clockOpenAt(clock ChannelClock, id string, at time.Time) bool {
	if clock == nil {
		return true
	}
	return clock.OpenAt(id, at)
}

// SetWaiter replaces the backoff primitive (tests only).
func (p *Pool) SetWaiter(fn func(ctx context.Context, delay time.Duration) error) {
	p.mu.Lock()
	p.waiter = fn
	p.mu.Unlock()
}

// ConcurrencyOf reports the live limit of one source.
func (p *Pool) ConcurrencyOf(id string) (current, max int, ok bool) {
	provider := p.find(id)
	if provider == nil || provider.limiter == nil {
		return 0, 0, false
	}
	return provider.limiter.Current(), provider.limiter.Max(), true
}

// OnSwitch installs a callback invoked outside the pool lock on real switches.
func (p *Pool) OnSwitch(fn func(from, to, reason string)) {
	p.mu.Lock()
	p.onSwitch = fn
	p.mu.Unlock()
}

// appendNote records a routing or throttling notice for the next UI poll and
// mirrors it into the diagnostic log (single choke point for these events).
func (p *Pool) appendNoteLog(text string) { applog.L().Infof("provider: %s", text) }

// appendNote records a routing or throttling notice for the next UI poll.
func (p *Pool) appendNote(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.notes) >= 64 {
		p.notes = append([]string(nil), p.notes[len(p.notes)-32:]...)
	}
	p.notes = append(p.notes, p.now().Format("15:04:05")+" "+text)
	p.appendNoteLog(text)
}

// DrainNotes returns and clears pending notices. The TUI calls it from its
// refresh loop so routing decisions appear in the event stream.
func (p *Pool) DrainNotes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.notes) == 0 {
		return nil
	}
	notes := p.notes
	p.notes = nil
	return notes
}

func (p *Pool) notifySwitch(from, to, reason string) {
	p.appendNote(fmt.Sprintf("源切换 %s → %s（%s）", from, to, reason))
	p.mu.RLock()
	fn := p.onSwitch
	p.mu.RUnlock()
	if fn != nil && from != to {
		fn(from, to, reason)
	}
}

// Statuses snapshots every provider for display.
func (p *Pool) Statuses() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.now()
	out := make([]Status, 0, len(p.providers))
	for _, provider := range p.providers {
		status := provider.quota.Status(now)
		status.ID, status.Label, status.Model = provider.ID(), provider.Label(), provider.Model()
		status.Vendor = provider.cfg.VendorOf()
		status.Plan = provider.cfg.Plan
		status.Enabled, status.Weight = provider.Enabled(), provider.cfg.Weight
		status.Active = provider.ID() == p.active
		status.Scheduled = clockOpenAt(p.clock, provider.ID(), now)
		status.DisabledReason = provider.DisabledReason()
		status.LastError = p.lastErr[provider.ID()]
		if provider.limiter != nil {
			status.Concurrency = provider.limiter.Current()
			status.Learned = provider.limiter.Learned()
			status.MaxConcur = provider.limiter.Max()
			status.InFlight = provider.limiter.InFlight()
			status.Throttles = provider.limiter.Throttles()
		}
		out = append(out, status)
	}
	return out
}

// SetEnabled toggles a source. Disabling the active source requires another
// enabled source, otherwise the pool would have nowhere to send work.
func (p *Pool) SetEnabled(id string, enabled bool) error {
	provider, err := p.byID(id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if !enabled {
		others := 0
		for _, candidate := range p.providers {
			if candidate.ID() != id && candidate.Enabled() {
				others++
			}
		}
		if others == 0 {
			p.mu.Unlock()
			return fmt.Errorf("不能停用最后一个启用的源")
		}
	}
	provider.setEnabled(enabled)
	if !enabled {
		provider.setDisabledReason("手动停用（/providers 里按 e 恢复）")
	}
	previous := p.active
	if !enabled && previous == id {
		p.active = p.orderLocked(nil)[0].ID()
		p.activeSince = p.now()
	}
	next := p.active
	p.mu.Unlock()
	if previous != next {
		p.notifySwitch(previous, next, "源 "+id+" 被停用")
	}
	return nil
}

// SetActive forces a provider; used by the manual override in /providers.
func (p *Pool) SetActive(id string) error {
	provider, err := p.byID(id)
	if err != nil {
		return err
	}
	if !provider.Enabled() {
		return fmt.Errorf("源 %s 已停用", id)
	}
	p.mu.Lock()
	previous := p.active
	p.active = id
	p.activeSince = p.now()
	p.mu.Unlock()
	p.notifySwitch(previous, id, "手动切换")
	// The user's choice survives a restart; a transparent failover must not.
	if err := p.SaveSelection(); err != nil {
		p.appendNote("保存模式选择失败：" + err.Error())
	}
	return nil
}

// activeProvider returns the current provider, falling back to any enabled one
// if the configured active source was disabled out of band.
func (p *Pool) activeProvider() *Provider {
	p.mu.RLock()
	id := p.active
	p.mu.RUnlock()
	if provider := p.find(id); provider != nil && provider.Enabled() {
		return provider
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if provider := p.find(p.active); provider != nil && provider.Enabled() {
		return provider
	}
	candidates := p.orderLocked(nil)
	if len(candidates) == 0 {
		return nil
	}
	previous := p.active
	p.active = candidates[0].ID()
	p.activeSince = p.now()
	go p.notifySwitch(previous, p.active, "当前源不可用")
	return candidates[0]
}

// Providers exposes immutable copies of provider identities for tests and UI.
func (p *Pool) Providers() []*Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Provider(nil), p.providers...)
}

func (p *Pool) String() string {
	var parts []string
	for _, status := range p.Statuses() {
		marker := ""
		if status.Active {
			marker = "*"
		}
		parts = append(parts, marker+status.ID)
	}
	return strings.Join(parts, ",")
}

// Compile-time proof that the pool is a drop-in model client.
var (
	_ llm.Client     = (*Pool)(nil)
	_ llm.ToolClient = (*Pool)(nil)
)

func (p *Pool) Chat(ctx context.Context, messages []llm.Message) (string, error) {
	content, _, err := runRequest(p, ctx, "chat", nil, func(requestCtx context.Context, client *llm.OpenAIClient) (string, error) {
		return client.Chat(requestCtx, messages)
	})
	if err != nil {
		return "", err
	}
	return content, nil
}

func (p *Pool) ChatStream(ctx context.Context, messages []llm.Message, emit func(llm.Delta) error) error {
	emitted := false
	_, providerID, err := runRequest(p, ctx, "stream", &emitted, func(requestCtx context.Context, client *llm.OpenAIClient) (struct{}, error) {
		return struct{}{}, client.ChatStream(requestCtx, messages, track(emit, &emitted))
	})
	_ = providerID
	return err
}

func (p *Pool) ChatTools(ctx context.Context, messages []llm.Message, tools []llm.ToolDefinition, emit func(llm.Delta) error) (llm.ToolResponse, error) {
	emitted := false
	response, providerID, err := runRequest(p, ctx, "tools", &emitted, func(requestCtx context.Context, client *llm.OpenAIClient) (llm.ToolResponse, error) {
		return client.ChatTools(requestCtx, messages, tools, track(emit, &emitted))
	})
	if err != nil {
		return llm.ToolResponse{}, err
	}
	p.rememberLineage(response.ResponseID, providerID)
	return response, nil
}

// track marks that model output was already surfaced, which forbids silent
// replay under another provider.
func track(emit func(llm.Delta) error, emitted *bool) func(llm.Delta) error {
	if emit == nil {
		return func(llm.Delta) error { return nil }
	}
	return func(delta llm.Delta) error {
		if delta.Content != "" || delta.Thinking != "" {
			*emitted = true
		}
		return emit(delta)
	}
}
