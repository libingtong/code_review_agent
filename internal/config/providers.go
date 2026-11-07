package config

import (
	"fmt"
	"strings"
	"time"
)

// ProviderConfig is one LLM source. Every source owns a complete OpenAIConfig
// because thinking modes, context limits and wire protocols differ per vendor;
// shared defaults are merged in at load time so a config only needs what varies.
type ProviderConfig struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	// Vendor drives probe/endpoint defaults; Plan selects the capacity tier.
	// Quota lives here because it belongs to the channel, not to the program:
	// two keys of the same vendor may hold different subscriptions.
	Vendor  string       `yaml:"vendor"`
	Plan    string       `yaml:"plan"`
	Enabled *bool        `yaml:"enabled"`
	Weight  int          `yaml:"weight"`
	OpenAI  OpenAIConfig `yaml:"openai"`
	Quota   QuotaConfig  `yaml:"quota"`
	// Schedule overrides the global working-hours policy for this channel only.
	// `enabled: false` means "always usable" (e.g. GLM around the clock while the
	// global window keeps MiniMax to the night).
	Schedule *ScheduleConfig `yaml:"schedule"`
}

func (p ProviderConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// QuotaConfig describes the subscription window and how to observe it.
type QuotaConfig struct {
	// StateFile is only read from the top-level quota section: window state must
	// survive restarts or a half-consumed 5h window looks fresh again.
	StateFile string `yaml:"state_file"`
	// LimitsFile persists runtime /limit changes (top-level section only).
	LimitsFile string           `yaml:"limits_file"`
	Window     string           `yaml:"window"`
	Probe      ProbeConfig      `yaml:"probe"`
	Local      LocalQuotaConfig `yaml:"local"`
	// MinHeadroom keeps a source out of rotation before it is fully drained,
	// so an in-flight audit is not cut off mid-turn.
	MinHeadroom float64 `yaml:"min_headroom"`
	// MaxUsageRatio caps how much of the window this program may consume, e.g.
	// 0.9 leaves 10% for other tools sharing the same subscription.
	MaxUsageRatio float64 `yaml:"max_usage_ratio"`
	// ReserveUnits is an absolute equivalent of MaxUsageRatio, useful for plans
	// measured in credits rather than percentages.
	ReserveUnits float64 `yaml:"reserve_units"`
}

// ProbeConfig is the observation strategy. "http" and "command" are best
// effort (vendors do not document quota APIs); "local" is pure accounting and
// always available as the fallback.
type ProbeConfig struct {
	Type            string            `yaml:"type"`
	IntervalSeconds int               `yaml:"interval_seconds"`
	TimeoutSeconds  int               `yaml:"timeout_seconds"`
	URL             string            `yaml:"url"`
	Method          string            `yaml:"method"`
	Headers         map[string]string `yaml:"headers"`
	Command         []string          `yaml:"command"`
	Regex           string            `yaml:"regex"`
	// Unit says what the probe reports: "" = native credits, "percent" = a 0-100
	// share of the window (GLM's TOKENS_LIMIT works this way). Percent readings
	// are converted into the channel's own unit using its configured capacity.
	Unit string     `yaml:"unit"`
	Bind BindConfig `yaml:"bind"`
}

// BindConfig lists candidate JSON paths, tried in order, so a vendor field
// rename is a config edit rather than a code change.
type BindConfig struct {
	// A path may be a literal: "=100" is the constant 100, which is how a
	// percentage-only payload gets a meaningful total.
	Used  []string `yaml:"used"`
	Total []string `yaml:"total"`
	Reset []string `yaml:"reset"`
	Plan  []string `yaml:"plan"`
}

// PeakConfig mirrors vendor off-peak discounts (GLM charges 50% off-peak).
type PeakConfig struct {
	Days  []string `yaml:"days"`
	Start string   `yaml:"start"`
	End   string   `yaml:"end"`
}

// LocalQuotaConfig is the accounting fallback used when probes are unavailable
// or when a vendor exposes no quota endpoint at all.
type LocalQuotaConfig struct {
	Unit                    string      `yaml:"unit"` // credits | tokens
	Total                   float64     `yaml:"total"`
	CreditsPerMillionTokens float64     `yaml:"credits_per_million_tokens"`
	Peak                    *PeakConfig `yaml:"peak"`
	PeakFactor              float64     `yaml:"peak_factor"`
	OffpeakFactor           float64     `yaml:"offpeak_factor"`
}

type RoutingConfig struct {
	SwitchBelow     float64 `yaml:"switch_below"`
	MinDwellSeconds int     `yaml:"min_dwell_seconds"`
	Prefer          string  `yaml:"prefer"`
}

// ConcurrencyConfig tunes adaptive in-flight request limiting. Subscription
// plans publish small concurrent-project limits, so the pool starts modest and
// halves on 429 (additive increase, multiplicative decrease).
type ConcurrencyConfig struct {
	MaxInFlight     int `yaml:"max_in_flight"`
	InitialInFlight int `yaml:"initial_in_flight"`
	MinInFlight     int `yaml:"min_in_flight"`
	// IncreaseAfterSuccesses is how many successful requests are needed before
	// raising the limit by IncreaseStep. Zero uses the default (20).
	IncreaseAfterSuccesses int `yaml:"increase_after_successes"`
	IncreaseStep           int `yaml:"increase_step"`
	// DecreaseFactor applies on 429; must be in (0,1). Zero uses 0.5.
	DecreaseFactor float64 `yaml:"decrease_factor"`
	// IncreaseCooldownSeconds blocks growth after a throttle event.
	IncreaseCooldownSeconds int `yaml:"increase_cooldown_seconds"`
}

const (
	defaultIncreaseAfter = 20
	defaultIncreaseStep  = 1
	defaultDecreaseRate  = 0.5
	defaultIncreaseCool  = 120
)

type ScheduleConfig struct {
	Enabled bool `yaml:"enabled"`
	// StoreFile holds runtime edits from /window so they survive a restart and
	// override the config file. Empty disables persistence.
	StoreFile     string             `yaml:"store_file"`
	Timezone      string             `yaml:"timezone"`
	Windows       []TimeWindowConfig `yaml:"windows"`
	OnWindowClose string             `yaml:"on_window_close"`
	PollSeconds   int                `yaml:"poll_seconds"`
}

type TimeWindowConfig struct {
	Start string   `yaml:"start"`
	End   string   `yaml:"end"`
	Days  []string `yaml:"days"`
}

type OrchestratorConfig struct {
	Enabled           bool   `yaml:"enabled"`
	TaskFile          string `yaml:"task_file"`
	StateFile         string `yaml:"state_file"`
	ReportDir         string `yaml:"report_dir"`
	SessionDir        string `yaml:"session_dir"`
	ContinueOnFailure bool   `yaml:"continue_on_failure"`
	Retries           int    `yaml:"retries"`
}

const DefaultQuotaWindow = 5 * time.Hour

// WindowDuration parses the subscription refresh window. Empty means 5h, which
// is what both GLM Coding Plan and MiniMax M Plan actually use.
func (q QuotaConfig) WindowDuration() (time.Duration, error) {
	value := strings.TrimSpace(q.Window)
	if value == "" {
		return DefaultQuotaWindow, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("quota.window must be a duration such as 5h: %w", err)
	}
	if duration < time.Minute || duration > 30*24*time.Hour {
		return 0, fmt.Errorf("quota.window must be between 1m and 720h")
	}
	return duration, nil
}

func (p ProbeConfig) Validate(section string) error {
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "", "auto", "local":
		return nil
	case "http":
		if strings.TrimSpace(p.URL) == "" {
			return fmt.Errorf("%s.probe.url is required for http probes", section)
		}
		if p.TimeoutSeconds < 0 || p.TimeoutSeconds > 300 {
			return fmt.Errorf("%s.probe.timeout_seconds must be between 0 and 300", section)
		}
	case "command":
		if len(p.Command) == 0 {
			return fmt.Errorf("%s.probe.command is required for command probes", section)
		}
	default:
		return fmt.Errorf("%s.probe.type must be http, command, local or auto", section)
	}
	if p.IntervalSeconds < 0 || p.IntervalSeconds > 86400 {
		return fmt.Errorf("%s.probe.interval_seconds must be between 0 and 86400", section)
	}
	return nil
}

func (p ProbeConfig) Interval() time.Duration {
	if p.IntervalSeconds <= 0 {
		return 2 * time.Minute
	}
	return time.Duration(p.IntervalSeconds) * time.Second
}

// IsPeak reports whether a moment falls inside the vendor's peak-pricing band.
func (l LocalQuotaConfig) IsPeak(at time.Time) bool {
	if l.Peak == nil || len(l.Peak.Days) == 0 {
		return true
	}
	day := strings.ToLower(at.Weekday().String()[:3])
	allowed := false
	for _, candidate := range l.Peak.Days {
		if strings.ToLower(strings.TrimSpace(candidate))[:min(3, len(strings.TrimSpace(candidate)))] == day {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	start, ok := parseClock(l.Peak.Start)
	if !ok {
		return false
	}
	end, ok := parseClock(l.Peak.End)
	if !ok {
		return false
	}
	minute := at.Hour()*60 + at.Minute()
	if start <= end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

func (l LocalQuotaConfig) Factor(at time.Time) float64 {
	if l.IsPeak(at) {
		if l.PeakFactor > 0 {
			return l.PeakFactor
		}
		return 1
	}
	if l.OffpeakFactor > 0 {
		return l.OffpeakFactor
	}
	return 1
}

func parseClock(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, err := parseSmallInt(parts[0])
	if err != nil || hour < 0 || hour > 24 {
		return 0, false
	}
	minute, err := parseSmallInt(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, false
	}
	if hour == 24 && minute != 0 {
		return 0, false
	}
	return hour*60 + minute, true
}

func parseSmallInt(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	total := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		total = total*10 + int(r-'0')
	}
	return total, nil
}

func validWeekday(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value))[:min(3, len(strings.TrimSpace(value)))] {
	case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// validateProviders normalises ids, merges inherited defaults and rejects
// configurations that cannot route deterministically.
func validateProviders(cfg *Config) error {
	if cfg.Quota.StateFile == "" {
		cfg.Quota.StateFile = "state/quota.json"
	}
	if cfg.Quota.LimitsFile == "" {
		cfg.Quota.LimitsFile = "state/limits.json"
	}
	if len(cfg.Providers) == 0 {
		legacy := cfg.OpenAI
		cfg.Providers = []ProviderConfig{{ID: "default", Label: "默认源", OpenAI: legacy, Quota: cfg.Quota, Weight: 1}}
	}
	seen := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		p.ID = strings.TrimSpace(p.ID)
		if p.ID == "" {
			return fmt.Errorf("providers[%d].id is required", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("providers[%d].id %q is duplicated", i, p.ID)
		}
		seen[p.ID] = true
		if strings.TrimSpace(p.Label) == "" {
			p.Label = p.ID
		}
		if p.Weight <= 0 {
			p.Weight = 1
		}
		if err := p.applyPlanPreset(); err != nil {
			return err
		}
		mergeOpenAIDefaults(&p.OpenAI, cfg.OpenAI)
		resolveAPIKey(&p.OpenAI)
		if err := validateReasoning(p.OpenAI, "providers."+p.ID+".openai"); err != nil {
			return err
		}
		if p.OpenAI.MaxContextTokens <= 0 || p.OpenAI.MaxOutputTokens <= 0 || p.OpenAI.MaxOutputTokens >= p.OpenAI.MaxContextTokens {
			return fmt.Errorf("providers.%s.openai token limits must be positive with output below context", p.ID)
		}
		if _, err := p.Quota.WindowDuration(); err != nil {
			return fmt.Errorf("providers.%s: %w", p.ID, err)
		}
		if err := p.Quota.Probe.Validate("providers." + p.ID); err != nil {
			return err
		}
		if p.Schedule != nil {
			if err := validateChannelSchedule(p.Schedule, cfg.Schedule.Timezone, "providers."+p.ID+".schedule"); err != nil {
				return err
			}
		}
		if p.Quota.MinHeadroom < 0 || p.Quota.MinHeadroom >= 1 {
			return fmt.Errorf("providers.%s.quota.min_headroom must be in [0,1)", p.ID)
		}
		if p.Quota.MaxUsageRatio < 0 || p.Quota.MaxUsageRatio > 1 {
			return fmt.Errorf("providers.%s.quota.max_usage_ratio must be in (0,1]; 1 means no self-imposed cap", p.ID)
		}
		if p.Quota.MaxUsageRatio == 0 {
			p.Quota.MaxUsageRatio = 1
		}
		if p.Quota.ReserveUnits < 0 {
			return fmt.Errorf("providers.%s.quota.reserve_units must be nonnegative", p.ID)
		}
		if p.Quota.Local.Total < 0 || p.Quota.Local.CreditsPerMillionTokens < 0 {
			return fmt.Errorf("providers.%s.quota.local values must be nonnegative", p.ID)
		}
		if p.Quota.Local.Peak != nil {
			if _, ok := parseClock(p.Quota.Local.Peak.Start); !ok {
				return fmt.Errorf("providers.%s.quota.local.peak.start must be HH:MM", p.ID)
			}
			if _, ok := parseClock(p.Quota.Local.Peak.End); !ok {
				return fmt.Errorf("providers.%s.quota.local.peak.end must be HH:MM", p.ID)
			}
			for _, day := range p.Quota.Local.Peak.Days {
				if !validWeekday(day) {
					return fmt.Errorf("providers.%s.quota.local.peak.days has invalid weekday %q", p.ID, day)
				}
			}
		}
	}
	enabled := 0
	for _, p := range cfg.Providers {
		if p.IsEnabled() {
			enabled++
		}
	}
	if enabled == 0 {
		return fmt.Errorf("at least one provider must be enabled")
	}
	return validateRouting(cfg)
}

func validateRouting(cfg *Config) error {
	r := &cfg.Routing
	if r.SwitchBelow == 0 {
		r.SwitchBelow = 0.15
	}
	if r.SwitchBelow < 0 || r.SwitchBelow >= 1 {
		return fmt.Errorf("routing.switch_below must be in (0,1)")
	}
	if r.MinDwellSeconds == 0 {
		r.MinDwellSeconds = 300
	}
	if r.MinDwellSeconds < 0 || r.MinDwellSeconds > 86400 {
		return fmt.Errorf("routing.min_dwell_seconds must be between 0 and 86400")
	}
	switch strings.ToLower(strings.TrimSpace(r.Prefer)) {
	case "", "sticky", "highest":
	default:
		return fmt.Errorf("routing.prefer must be sticky or highest")
	}
	if r.Prefer == "" {
		r.Prefer = "sticky"
	}
	return validateConcurrency(cfg)
}

// WithDefaults fills unset fields. NewPool calls it so directly constructed
// Config values (embedding, tests) behave like a validated file.
func (c ConcurrencyConfig) WithDefaults(workers int) ConcurrencyConfig {
	if workers < 2 {
		workers = 2
	}
	if c.MaxInFlight == 0 {
		c.MaxInFlight = workers
	}
	if c.MinInFlight == 0 {
		c.MinInFlight = 1
	}
	if c.InitialInFlight == 0 {
		c.InitialInFlight = minInt(4, c.MaxInFlight)
	}
	if c.InitialInFlight < c.MinInFlight {
		c.InitialInFlight = c.MinInFlight
	}
	if c.InitialInFlight > c.MaxInFlight {
		c.InitialInFlight = c.MaxInFlight
	}
	if c.IncreaseAfterSuccesses == 0 {
		c.IncreaseAfterSuccesses = defaultIncreaseAfter
	}
	if c.IncreaseStep == 0 {
		c.IncreaseStep = defaultIncreaseStep
	}
	if c.DecreaseFactor == 0 {
		c.DecreaseFactor = defaultDecreaseRate
	}
	if c.IncreaseCooldownSeconds == 0 {
		c.IncreaseCooldownSeconds = defaultIncreaseCool
	}
	return c
}

func validateConcurrency(cfg *Config) error {
	c := &cfg.Concurrency
	workers := cfg.Agent.ReconAgents + cfg.Agent.AuditAgents + 1
	if c.MaxInFlight == 0 {
		// Every worker issues at most one request at a time, so the team size is
		// the natural ceiling.
		c.MaxInFlight = workers
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 512 {
		return fmt.Errorf("concurrency.max_in_flight must be between 1 and 512")
	}
	if c.MinInFlight == 0 {
		c.MinInFlight = 1
	}
	if c.MinInFlight < 1 || c.MinInFlight > c.MaxInFlight {
		return fmt.Errorf("concurrency.min_in_flight must be between 1 and max_in_flight")
	}
	if c.InitialInFlight == 0 {
		// Start below the ceiling: discovering the real limit downwards is much
		// cheaper than triggering a burst of 429s first.
		c.InitialInFlight = minInt(4, c.MaxInFlight)
	}
	if c.InitialInFlight < c.MinInFlight || c.InitialInFlight > c.MaxInFlight {
		return fmt.Errorf("concurrency.initial_in_flight must be between min_in_flight and max_in_flight")
	}
	if c.IncreaseAfterSuccesses == 0 {
		c.IncreaseAfterSuccesses = defaultIncreaseAfter
	}
	if c.IncreaseAfterSuccesses < 1 || c.IncreaseAfterSuccesses > 100000 {
		return fmt.Errorf("concurrency.increase_after_successes must be between 1 and 100000")
	}
	if c.IncreaseStep == 0 {
		c.IncreaseStep = defaultIncreaseStep
	}
	if c.IncreaseStep < 1 || c.IncreaseStep > 64 {
		return fmt.Errorf("concurrency.increase_step must be between 1 and 64")
	}
	if c.DecreaseFactor == 0 {
		c.DecreaseFactor = defaultDecreaseRate
	}
	if c.DecreaseFactor <= 0 || c.DecreaseFactor >= 1 {
		return fmt.Errorf("concurrency.decrease_factor must be in (0,1)")
	}
	if c.IncreaseCooldownSeconds == 0 {
		c.IncreaseCooldownSeconds = defaultIncreaseCool
	}
	if c.IncreaseCooldownSeconds < 0 || c.IncreaseCooldownSeconds > 86400 {
		return fmt.Errorf("concurrency.increase_cooldown_seconds must be between 0 and 86400")
	}
	return validateSchedule(cfg)
}

// validateChannelSchedule reuses the global rules but inherits the timezone and
// never owns persistence (the store file is global).
func validateChannelSchedule(s *ScheduleConfig, inheritedZone, section string) error {
	if !s.Enabled {
		// Explicitly disabled: the channel ignores the global window entirely.
		s.Windows = nil
		if s.Timezone == "" {
			s.Timezone = inheritedZone
		}
		return nil
	}
	if s.Timezone == "" {
		s.Timezone = inheritedZone
	}
	if s.Timezone == "" {
		s.Timezone = "Asia/Shanghai"
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("%s.timezone %q 无效：%w", section, s.Timezone, err)
	}
	if len(s.Windows) == 0 {
		return fmt.Errorf("%s.enabled 需要至少一个窗口", section)
	}
	for i := range s.Windows {
		w := &s.Windows[i]
		if _, ok := parseClock(w.Start); !ok {
			return fmt.Errorf("%s.windows[%d].start 必须是 HH:MM", section, i)
		}
		if _, ok := parseClock(w.End); !ok {
			return fmt.Errorf("%s.windows[%d].end 必须是 HH:MM", section, i)
		}
		if len(w.Days) == 0 {
			w.Days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
		}
		for _, day := range w.Days {
			if !validWeekday(day) {
				return fmt.Errorf("%s.windows[%d].days 含无效星期 %q", section, i, day)
			}
		}
	}
	return nil
}

func validateSchedule(cfg *Config) error {
	s := &cfg.Schedule
	if !s.Enabled {
		return nil
	}
	if s.Timezone == "" {
		s.Timezone = "Asia/Shanghai"
	}
	if s.StoreFile == "" {
		s.StoreFile = "state/schedule.json"
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("schedule.timezone %q is not a valid IANA zone: %w", s.Timezone, err)
	}
	if len(s.Windows) == 0 {
		return fmt.Errorf("schedule.enabled requires at least one window")
	}
	if s.PollSeconds == 0 {
		s.PollSeconds = 30
	}
	if s.PollSeconds < 5 || s.PollSeconds > 3600 {
		return fmt.Errorf("schedule.poll_seconds must be between 5 and 3600")
	}
	switch strings.ToLower(strings.TrimSpace(s.OnWindowClose)) {
	case "", "pause":
		s.OnWindowClose = "pause"
	case "finish_current_task":
		s.OnWindowClose = "finish_current_task"
	default:
		return fmt.Errorf("schedule.on_window_close must be pause or finish_current_task")
	}
	for i := range s.Windows {
		w := &s.Windows[i]
		if _, ok := parseClock(w.Start); !ok {
			return fmt.Errorf("schedule.windows[%d].start must be HH:MM", i)
		}
		if _, ok := parseClock(w.End); !ok {
			return fmt.Errorf("schedule.windows[%d].end must be HH:MM", i)
		}
		if len(w.Days) == 0 {
			w.Days = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
		}
		for _, day := range w.Days {
			if !validWeekday(day) {
				return fmt.Errorf("schedule.windows[%d].days has invalid weekday %q", i, day)
			}
		}
	}
	return nil
}

// mergeOpenAIDefaults fills a provider section from the global openai section so
// a multi-source config only states what differs.
func mergeOpenAIDefaults(dst *OpenAIConfig, base OpenAIConfig) {
	if dst.BaseURL == "" {
		dst.BaseURL = base.BaseURL
	}
	if dst.APIInterface == "" {
		dst.APIInterface = base.APIInterface
	}
	if dst.APIKey == "" && dst.APIKeyEnv == "" {
		dst.APIKey, dst.APIKeyEnv = base.APIKey, base.APIKeyEnv
	}
	if dst.Model == "" {
		dst.Model = base.Model
	}
	if dst.Thinking == nil {
		dst.Thinking = base.Thinking
	}
	if dst.ReasoningEffort == "" {
		dst.ReasoningEffort = base.ReasoningEffort
	}
	if dst.Temperature == 0 {
		dst.Temperature = base.Temperature
	}
	if dst.TopP == 0 {
		dst.TopP = base.TopP
	}
	if dst.MaxContextTokens == 0 {
		dst.MaxContextTokens = base.MaxContextTokens
	}
	if dst.MaxOutputTokens == 0 {
		dst.MaxOutputTokens = base.MaxOutputTokens
	}
	if dst.TimeoutSeconds == 0 {
		dst.TimeoutSeconds = base.TimeoutSeconds
	}
}

// EffectiveTokenLimits returns the tightest window across enabled sources. A
// request sized for the largest provider would be rejected elsewhere, so all
// compression decisions use this pair.
func (cfg Config) EffectiveTokenLimits() (MinCompressionSource, error) {
	// Directly constructed Config values (tests, embedded callers) may skip
	// provider synthesis, in which case the legacy section is authoritative.
	if len(cfg.Providers) == 0 {
		if cfg.OpenAI.MaxContextTokens <= 0 || cfg.OpenAI.MaxOutputTokens <= 0 || cfg.OpenAI.MaxOutputTokens >= cfg.OpenAI.MaxContextTokens {
			return MinCompressionSource{}, fmt.Errorf("openai token limits must be positive with output below context")
		}
		return MinCompressionSource{Context: cfg.OpenAI.MaxContextTokens, Output: cfg.OpenAI.MaxOutputTokens, Source: "openai"}, nil
	}
	best := MinCompressionSource{}
	for _, p := range cfg.Providers {
		if !p.IsEnabled() {
			continue
		}
		if best.Context == 0 || p.OpenAI.MaxContextTokens < best.Context {
			best = MinCompressionSource{Context: p.OpenAI.MaxContextTokens, Output: p.OpenAI.MaxOutputTokens, Source: p.ID}
		}
	}
	if best.Context == 0 {
		return MinCompressionSource{}, fmt.Errorf("no enabled provider defines token limits")
	}
	if best.Output <= 0 || best.Output >= best.Context {
		return MinCompressionSource{}, fmt.Errorf("providers.%s output must be positive and below context", best.Source)
	}
	return best, nil
}
