package config

import (
	"strings"
	"testing"
	"time"
)

func TestLegacyOpenAISectionSynthesisesSingleProvider(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "openai:\n  base_url: https://api.example.com/v1\n  api_key: sk-abc\n  model: m1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want 1 synthesised legacy source", len(cfg.Providers))
	}
	got := cfg.Providers[0]
	if got.ID != "default" || !got.IsEnabled() || got.OpenAI.Model != "m1" || got.OpenAI.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("legacy source not mirrored: %+v", got)
	}
	if got.Quota.MinHeadroom != 0 {
		t.Fatalf("legacy source must not invent headroom: %v", got.Quota.MinHeadroom)
	}
}

func TestProvidersInheritSharedDefaults(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai:
  base_url: https://api.example.com/v1
  api_key: sk-shared
  model: base-model
  max_context_tokens: 200000
  max_output_tokens: 8192
providers:
  - id: a
    openai:
      model: a-model
  - id: b
    openai:
      model: b-model
      max_context_tokens: 64000
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d", len(cfg.Providers))
	}
	a, b := cfg.Providers[0], cfg.Providers[1]
	if a.OpenAI.BaseURL != "https://api.example.com/v1" || a.OpenAI.APIKey != "sk-shared" || a.OpenAI.MaxContextTokens != 200000 {
		t.Fatalf("provider a did not inherit shared defaults: %+v", a.OpenAI)
	}
	if b.OpenAI.MaxContextTokens != 64000 {
		t.Fatalf("explicit provider limit was overwritten: %d", b.OpenAI.MaxContextTokens)
	}
	if a.Weight != 1 || b.Weight != 1 || a.Label != "a" {
		t.Fatalf("provider defaults wrong: %+v %+v", a, b)
	}
}

func TestEffectiveTokenLimitsUsesTightestSource(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai:
  base_url: https://api.example.com/v1
  api_key: sk-shared
  model: m
  max_context_tokens: 1000000
  max_output_tokens: 65536
providers:
  - id: wide
    openai:
      model: wide
  - id: narrow
    openai:
      model: narrow
      max_context_tokens: 200000
      max_output_tokens: 32768
`)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := cfg.EffectiveTokenLimits()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Source != "narrow" || limits.Context != 200000 || limits.Output != 32768 {
		t.Fatalf("effective limits picked the wrong source: %+v", limits)
	}
	threshold, err := cfg.CompressionThreshold()
	if err != nil {
		t.Fatal(err)
	}
	if threshold >= 200000 || threshold <= 0 {
		t.Fatalf("compression threshold must be bounded by the narrow source: %d", threshold)
	}
}

func TestProviderValidationRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"duplicate id": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - {id: same}
  - {id: same}
`,
		"all disabled": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - {id: a, enabled: false}
`,
		"bad window": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - id: a
    quota: {window: "5 hours"}
`,
		"bad probe": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - id: a
    quota:
      probe: {type: http}
`,
		"output above context": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m, max_context_tokens: 1000, max_output_tokens: 2000}
`,
		"bad schedule": `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
schedule:
  enabled: true
  windows:
    - {start: "9pm", end: "08:00"}
`,
	}
	for name, text := range cases {
		if _, err := loadBudgetConfig(t, text); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestScheduleDefaultsAndWindowValidation(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
schedule:
  enabled: true
  windows:
    - {start: "21:00", end: "08:00"}
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Schedule.Timezone != "Asia/Shanghai" || cfg.Schedule.PollSeconds != 30 || cfg.Schedule.OnWindowClose != "pause" {
		t.Fatalf("schedule defaults missing: %+v", cfg.Schedule)
	}
	if len(cfg.Schedule.Windows[0].Days) != 7 {
		t.Fatalf("default weekday set should cover the week: %v", cfg.Schedule.Windows[0].Days)
	}
}

func TestLocalQuotaPeakFactor(t *testing.T) {
	local := LocalQuotaConfig{
		Total:         12000,
		PeakFactor:    1,
		OffpeakFactor: 0.5,
		Peak:          &PeakConfig{Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "14:00", End: "18:00"},
	}
	// 2026-11-09 is a Monday.
	peak := time.Date(2026, 11, 9, 15, 30, 0, 0, time.UTC)
	offpeak := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	weekend := time.Date(2026, 11, 7, 15, 30, 0, 0, time.UTC)
	if !local.IsPeak(peak) || local.Factor(peak) != 1 {
		t.Fatal("weekday afternoon must be peak")
	}
	if local.IsPeak(offpeak) || local.Factor(offpeak) != 0.5 {
		t.Fatal("night must be off-peak with the half discount")
	}
	if local.IsPeak(weekend) {
		t.Fatal("weekend must not be peak")
	}
}

func TestRoutingDefaults(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.SwitchBelow != 0.15 || cfg.Routing.MinDwellSeconds != 300 || cfg.Routing.Prefer != "sticky" {
		t.Fatalf("routing defaults missing: %+v", cfg.Routing)
	}
	if cfg.Quota.StateFile != "state/quota.json" {
		t.Fatalf("quota state path default missing: %q", cfg.Quota.StateFile)
	}
}

func TestProviderConfigRequiresIndependentKeys(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai:
  base_url: https://api.example.com/v1
  api_key_env: SHARED_KEY
  model: m
providers:
  - id: glm
    openai:
      base_url: https://open.bigmodel.cn/api/coding/paas/v4
      api_key: sk-glm
      model: glm-5.3
  - id: mm
    openai:
      base_url: https://api.minimax.cn/v1
      api_key: sk-mm
      model: MiniMax-M3.1-Flash
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].OpenAI.APIKey != "sk-glm" || cfg.Providers[1].OpenAI.APIKey != "sk-mm" {
		t.Fatal("per-source keys must not be merged")
	}
	if cfg.Providers[0].OpenAI.APIKeyEnv != "" {
		t.Fatal("an explicit per-source key must not drag in the shared env name")
	}
	if strings.Contains(cfg.Providers[0].OpenAI.BaseURL, "minimax") {
		t.Fatal("base urls must stay per source")
	}
}

func TestConcurrencyDefaultsFollowTeamSize(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
agent: {recon_agents: 4, audit_agents: 12}
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.MaxInFlight != 17 {
		t.Fatalf("max_in_flight = %d, want recon+audit+moderator = 17", cfg.Concurrency.MaxInFlight)
	}
	if cfg.Concurrency.MinInFlight != 1 || cfg.Concurrency.InitialInFlight != 4 {
		t.Fatalf("initial/min wrong: %+v", cfg.Concurrency)
	}
	if cfg.Concurrency.DecreaseFactor != 0.5 || cfg.Concurrency.IncreaseAfterSuccesses != 20 {
		t.Fatalf("AIMD defaults wrong: %+v", cfg.Concurrency)
	}
}

func TestConcurrencyDefaultsKeepInitialWithinBounds(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
agent: {recon_agents: 1, audit_agents: 1}
concurrency: {max_in_flight: 2, min_in_flight: 2}
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Concurrency.InitialInFlight != 2 {
		t.Fatalf("initial must be clamped into [min,max]: %d", cfg.Concurrency.InitialInFlight)
	}
}

func TestConcurrencyValidation(t *testing.T) {
	cases := map[string]string{
		"initial above max":  "concurrency: {max_in_flight: 4, initial_in_flight: 9}",
		"min above max":      "concurrency: {max_in_flight: 3, min_in_flight: 5}",
		"bad decrease":       "concurrency: {decrease_factor: 1.2}",
		"zero increase step": "concurrency: {increase_after_successes: 5, increase_step: -1}",
	}
	for name, block := range cases {
		text := "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\n" + block + "\n"
		if _, err := loadBudgetConfig(t, text); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
}

func TestUsageCapDefaultsAndValidation(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].Quota.MaxUsageRatio != 1 {
		t.Fatalf("default cap must be 100%%, got %v", cfg.Providers[0].Quota.MaxUsageRatio)
	}
	configured, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - id: glm
    quota: {max_usage_ratio: 0.9, reserve_units: 500}
`)
	if err != nil {
		t.Fatal(err)
	}
	if configured.Providers[0].Quota.MaxUsageRatio != 0.9 || configured.Providers[0].Quota.ReserveUnits != 500 {
		t.Fatalf("cap not parsed: %+v", configured.Providers[0].Quota)
	}
	for _, block := range []string{"quota: {max_usage_ratio: 1.5}", "quota: {reserve_units: -1}"} {
		text := "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\nproviders:\n  - id: glm\n    " + block + "\n"
		if _, err := loadBudgetConfig(t, text); err == nil {
			t.Fatalf("%s: expected validation error", block)
		}
	}
}

func TestChannelScheduleOverride(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
schedule:
  enabled: true
  windows: [{start: "21:00", end: "08:00"}]
providers:
  - id: glm
    vendor: glm
    schedule: {enabled: false}
  - id: minimax
    vendor: minimax
    schedule:
      enabled: true
      windows: [{start: "09:00", end: "18:00", days: [mon, tue, wed, thu, fri]}]
`)
	if err != nil {
		t.Fatal(err)
	}
	glm, mm := cfg.Providers[0], cfg.Providers[1]
	if glm.Schedule == nil || glm.Schedule.Enabled {
		t.Fatalf("glm should override to always: %+v", glm.Schedule)
	}
	if len(glm.Schedule.Windows) != 0 {
		t.Fatalf("a disabled override must not keep windows: %+v", glm.Schedule.Windows)
	}
	if mm.Schedule == nil || !mm.Schedule.Enabled || mm.Schedule.Windows[0].Start != "09:00" {
		t.Fatalf("channel window not parsed: %+v", mm.Schedule)
	}
	if mm.Schedule.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone must inherit the global one: %q", mm.Schedule.Timezone)
	}
}

func TestChannelScheduleValidation(t *testing.T) {
	_, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: m}
providers:
  - id: glm
    schedule:
      enabled: true
      windows: [{start: "9pm", end: "08:00"}]
`)
	if err == nil {
		t.Fatal("invalid channel clock must be rejected")
	}
}
