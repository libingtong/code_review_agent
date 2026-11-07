package config

import "testing"

func TestPlanPresetFillsChannelCapacity(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://api.minimax.cn/v1", api_key: sk-base, model: base}
providers:
  - id: glm-pro
    vendor: glm
    plan: glm-pro
`)
	if err != nil {
		t.Fatal(err)
	}
	channel := cfg.Providers[0]
	if channel.Quota.Local.Total != 12000 || channel.Quota.Local.Unit != "credits" {
		t.Fatalf("pro tier capacity not applied: %+v", channel.Quota.Local)
	}
	if channel.OpenAI.Model != "glm-5.3" || channel.OpenAI.MaxContextTokens != 200000 {
		t.Fatalf("endpoint defaults not applied: %+v", channel.OpenAI)
	}
	if channel.Quota.Probe.Type != "http" || channel.Quota.Probe.URL == "" {
		t.Fatalf("probe preset missing: %+v", channel.Quota.Probe)
	}
	if channel.Quota.Local.Peak == nil || channel.Quota.Local.OffpeakFactor != 0.5 {
		t.Fatalf("pricing preset missing: %+v", channel.Quota.Local)
	}
	if got := channel.VendorOf(); got != "glm" {
		t.Fatalf("vendor = %q", got)
	}
}

func TestPlanTiersDifferInCapacity(t *testing.T) {
	capacities := map[string]float64{}
	for _, plan := range []string{"glm-lite", "glm-pro", "glm-max"} {
		cfg, err := loadBudgetConfig(t, "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\nproviders:\n  - id: "+plan+"\n    vendor: glm\n    plan: "+plan+"\n")
		if err != nil {
			t.Fatal(err)
		}
		capacities[plan] = cfg.Providers[0].Quota.Local.Total
	}
	if !(capacities["glm-lite"] < capacities["glm-pro"] && capacities["glm-pro"] < capacities["glm-max"]) {
		t.Fatalf("plan tiers must differ: %+v", capacities)
	}
	if capacities["glm-lite"] != 2000 || capacities["glm-max"] != 28000 {
		t.Fatalf("published tier capacities drifted: %+v", capacities)
	}
}

func TestExplicitValuesBeatPreset(t *testing.T) {
	cfg, err := loadBudgetConfig(t, `
openai: {base_url: "https://x/v1", api_key: sk-1, model: base}
providers:
  - id: glm
    vendor: glm
    plan: glm-lite
    openai:
      model: glm-5.3-flash
      max_context_tokens: 64000
      max_output_tokens: 32000
    quota:
      local:
        total: 5000
        unit: tokens
`)
	if err != nil {
		t.Fatal(err)
	}
	channel := cfg.Providers[0]
	if channel.Quota.Local.Total != 5000 || channel.Quota.Local.Unit != "tokens" {
		t.Fatalf("explicit capacity overridden by preset: %+v", channel.Quota.Local)
	}
	if channel.OpenAI.Model != "glm-5.3-flash" || channel.OpenAI.MaxContextTokens != 64000 {
		t.Fatalf("explicit model limits overridden: %+v", channel.OpenAI)
	}
}

func TestUnknownPlanIsRejected(t *testing.T) {
	_, err := loadBudgetConfig(t, "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\nproviders:\n  - id: x\n    plan: glm-ultra\n")
	if err == nil {
		t.Fatal("unknown plan must fail loudly")
	}
}

func TestVendorWithoutPlanStillGetsProbeDefaults(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "openai: {base_url: \"https://x/v1\", api_key: sk-1, model: m}\nproviders:\n  - id: glm2\n    vendor: glm\n")
	if err != nil {
		t.Fatal(err)
	}
	channel := cfg.Providers[0]
	if channel.OpenAI.BaseURL == "" || channel.Quota.Probe.Type != "http" {
		t.Fatalf("vendor defaults missing: %+v %+v", channel.OpenAI, channel.Quota.Probe)
	}
	if channel.Quota.Local.Total != 0 {
		t.Fatalf("without a plan the capacity must stay unknown, got %v", channel.Quota.Local.Total)
	}
}

func TestVendorInferredFromEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://open.bigmodel.cn/api/coding/paas/v4": "glm",
		"https://api.minimax.cn/v1":                   "minimax",
		"https://api.openai.com/v1":                   "openai",
		"https://gateway.internal/llm":                "custom",
	}
	for base, want := range cases {
		provider := ProviderConfig{OpenAI: OpenAIConfig{BaseURL: base}}
		if got := provider.VendorOf(); got != want {
			t.Fatalf("%s: vendor = %q want %q", base, got, want)
		}
	}
}
