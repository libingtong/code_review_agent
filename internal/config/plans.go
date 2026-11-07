package config

import (
	"fmt"
	"strings"
)

// A "channel" is one concrete way to reach a vendor: a base URL plus an API key
// plus a subscription plan. Quota belongs to the channel, not to the program, so
// two channels of the same vendor (two keys, two plans) must be able to report
// completely different remaining quota.
//
// Plan presets exist because the plan, not the vendor, decides the capacity:
// GLM Lite and GLM Max share an endpoint but differ 14x in 5 hour credits.
type PlanPreset struct {
	Name    string
	Vendor  string
	Label   string
	BaseURL string
	Model   string
	// ContextTokens/OutputTokens are only applied when the vendor differs from
	// the global default, so one plan cannot silently shrink another channel.
	ContextTokens int
	OutputTokens  int
	// Credits is the 5 hour capacity of this plan in the channel's own unit.
	Credits           float64
	Unit              string
	CreditsPerMillion float64
	Peak              *PeakConfig
	PeakFactor        float64
	OffpeakFactor     float64
	Probe             ProbeConfig
}

// glmProbeBind is shared by every GLM tier; only the capacity differs.
// glmProbeBind matches the live /api/monitor/usage/quota/limit payload:
// TOKENS_LIMIT carries only a percentage plus nextResetTime, while data.level
// names the subscribed tier ("lite"/"pro"/"max").
var glmProbeBind = BindConfig{
	Used:  []string{"data.limits[type=TOKENS_LIMIT].percentage", "data.limits[type=TOKENS_LIMIT].currentValue"},
	Total: []string{"=100", "data.limits[type=TOKENS_LIMIT].limit"},
	Reset: []string{"data.limits[type=TOKENS_LIMIT].nextResetTime"},
	Plan:  []string{"data.level"},
}

func glmPlan(name, label string, credits float64) PlanPreset {
	return PlanPreset{
		Name: name, Vendor: "glm", Label: label,
		BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4", Model: "glm-5.3",
		ContextTokens: 200000, OutputTokens: 65536,
		// Fallback metering uses GLM-5.3-Flash coefficients (2.3 input / 0.56 cached
		// / 8 output per 10k), the cheaper model this project actually runs. The
		// conservative 5.3 rate would overstate consumption by ~3x.
		Credits: credits, Unit: "credits", CreditsPerMillion: 515,
		Peak:          &PeakConfig{Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "14:00", End: "18:00"},
		PeakFactor:    1,
		OffpeakFactor: 0.5,
		Probe: ProbeConfig{
			Type: "http", IntervalSeconds: 120, TimeoutSeconds: 15, Unit: "percent",
			URL:     "https://open.bigmodel.cn/api/monitor/usage/quota/limit",
			Headers: map[string]string{"Authorization": "Bearer ${api_key}"},
			Bind:    glmProbeBind,
		},
	}
}

// PlanPresets is the built-in table. Adding a vendor tier is one entry here,
// never a code change elsewhere.
func PlanPresets() map[string]PlanPreset {
	presets := map[string]PlanPreset{
		"glm-lite": glmPlan("glm-lite", "GLM Coding Plan Lite", 2000),
		"glm-pro":  glmPlan("glm-pro", "GLM Coding Plan Pro", 12000),
		"glm-max":  glmPlan("glm-max", "GLM Coding Plan Max", 28000),
		"minimax-mplan": {
			Name: "minimax-mplan", Vendor: "minimax", Label: "MiniMax M Plan",
			BaseURL: "https://api.minimax.cn/v1", Model: "MiniMax-M3.1-Flash",
			Unit: "percent", Credits: 100,
			Probe: ProbeConfig{
				Type: "command", IntervalSeconds: 300, TimeoutSeconds: 20, Unit: "percent",
				Command: []string{"mmx", "quota", "--output", "json"},
				Bind: BindConfig{
					Used:  []string{"data.used_percent", "usage.used"},
					Total: []string{"data.total_percent", "usage.total"},
					Reset: []string{"data.reset_at", "usage.window_end"},
				},
			},
		},
	}
	for key, preset := range presets {
		presets[key] = preset
	}
	return presets
}

// applyPlanPreset fills only what the provider left empty, so an explicit value
// in YAML always beats the preset.
func (p *ProviderConfig) applyPlanPreset() error {
	if p.Plan == "" {
		// Convenience: an id of "glm" plus no plan still gets GLM's defaults.
		if p.Vendor == "" && strings.HasPrefix(strings.ToLower(p.ID), "glm") {
			p.Vendor = "glm"
		}
		applyVendorDefaults(p)
		return nil
	}
	preset, ok := PlanPresets()[strings.ToLower(strings.TrimSpace(p.Plan))]
	if !ok {
		return fmt.Errorf("providers.%s.plan %q 未知；可用：%s", p.ID, p.Plan, strings.Join(presetNames(), ", "))
	}
	if p.Vendor == "" {
		p.Vendor = preset.Vendor
	}
	if p.Label == "" || p.Label == p.ID {
		p.Label = preset.Label
	}
	if p.OpenAI.BaseURL == "" {
		p.OpenAI.BaseURL = preset.BaseURL
	}
	if p.OpenAI.Model == "" {
		p.OpenAI.Model = preset.Model
	}
	if p.OpenAI.MaxContextTokens == 0 && preset.ContextTokens > 0 {
		p.OpenAI.MaxContextTokens = preset.ContextTokens
	}
	if p.OpenAI.MaxOutputTokens == 0 && preset.OutputTokens > 0 {
		p.OpenAI.MaxOutputTokens = preset.OutputTokens
	}
	if p.Quota.Local.Total == 0 && preset.Credits > 0 {
		p.Quota.Local.Total = preset.Credits
	}
	if p.Quota.Local.Unit == "" && preset.Unit != "" {
		p.Quota.Local.Unit = preset.Unit
	}
	if p.Quota.Local.CreditsPerMillionTokens == 0 && preset.CreditsPerMillion > 0 {
		p.Quota.Local.CreditsPerMillionTokens = preset.CreditsPerMillion
	}
	if p.Quota.Local.Peak == nil && preset.Peak != nil {
		peak := *preset.Peak
		peak.Days = append([]string(nil), preset.Peak.Days...)
		p.Quota.Local.Peak = &peak
	}
	if p.Quota.Local.PeakFactor == 0 && preset.PeakFactor > 0 {
		p.Quota.Local.PeakFactor = preset.PeakFactor
	}
	if p.Quota.Local.OffpeakFactor == 0 && preset.OffpeakFactor > 0 {
		p.Quota.Local.OffpeakFactor = preset.OffpeakFactor
	}
	if p.Quota.Probe.Type == "" && p.Quota.Probe.URL == "" && len(p.Quota.Probe.Command) == 0 {
		probe := preset.Probe
		if len(probe.Headers) > 0 {
			copied := make(map[string]string, len(probe.Headers))
			for k, v := range probe.Headers {
				copied[k] = v
			}
			probe.Headers = copied
		}
		probe.Command = append([]string(nil), probe.Command...)
		p.Quota.Probe = probe
	}
	return nil
}

// applyVendorDefaults gives a plan-less channel sensible probe defaults so
// "just add a second GLM key" works without repeating the whole quota block.
func applyVendorDefaults(p *ProviderConfig) {
	switch strings.ToLower(strings.TrimSpace(p.Vendor)) {
	case "glm", "zhipu", "bigmodel", "z.ai", "zai":
		p.Vendor = "glm"
		if p.OpenAI.BaseURL == "" {
			p.OpenAI.BaseURL = "https://open.bigmodel.cn/api/coding/paas/v4"
		}
		if p.OpenAI.Model == "" {
			p.OpenAI.Model = "glm-5.3"
		}
		if p.OpenAI.MaxContextTokens == 0 {
			p.OpenAI.MaxContextTokens = 200000
		}
		if p.OpenAI.MaxOutputTokens == 0 {
			p.OpenAI.MaxOutputTokens = 65536
		}
		if p.Quota.Probe.Type == "" && p.Quota.Probe.URL == "" {
			p.Quota.Probe = ProbeConfig{Type: "http", IntervalSeconds: 120, TimeoutSeconds: 15, Unit: "percent",
				URL:     "https://open.bigmodel.cn/api/monitor/usage/quota/limit",
				Headers: map[string]string{"Authorization": "Bearer ${api_key}"},
				Bind:    glmProbeBind}
		}
	case "minimax", "mm":
		p.Vendor = "minimax"
		if p.OpenAI.BaseURL == "" {
			p.OpenAI.BaseURL = "https://api.minimax.cn/v1"
		}
		if p.OpenAI.Model == "" {
			p.OpenAI.Model = "MiniMax-M3.1-Flash"
		}
		if p.OpenAI.MaxContextTokens == 0 {
			p.OpenAI.MaxContextTokens = 1000000
		}
		if p.OpenAI.MaxOutputTokens == 0 {
			p.OpenAI.MaxOutputTokens = 65536
		}
		if p.Quota.Probe.Type == "" && len(p.Quota.Probe.Command) == 0 {
			p.Quota.Probe = ProbeConfig{Type: "command", IntervalSeconds: 300, TimeoutSeconds: 20, Unit: "percent",
				Command: []string{"mmx", "quota", "--output", "json"},
				Bind: BindConfig{
					Used:  []string{"data.used_percent", "usage.used"},
					Total: []string{"data.total_percent", "usage.total"},
					Reset: []string{"data.reset_at", "usage.window_end"},
				}}
		}
	default:
		if p.Vendor == "" {
			p.Vendor = "custom"
		}
	}
}

func presetNames() []string {
	names := make([]string, 0, 4)
	for name := range PlanPresets() {
		names = append(names, name)
	}
	// Stable, predictable order in error messages.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// VendorOf reports the effective vendor of a channel (explicit field, else
// guessed from the endpoint).
func (p ProviderConfig) VendorOf() string {
	if p.Vendor != "" {
		return p.Vendor
	}
	base := strings.ToLower(p.OpenAI.BaseURL)
	switch {
	case strings.Contains(base, "bigmodel"), strings.Contains(base, "z.ai"):
		return "glm"
	case strings.Contains(base, "minimax"):
		return "minimax"
	case strings.Contains(base, "openai"):
		return "openai"
	case base == "":
		return "custom"
	}
	return "custom"
}
