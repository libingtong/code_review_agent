package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"code-review-agent/internal/config"
)

// QuotaReader is what the UI and the scheduler need from quota observation.
type QuotaReader interface {
	Statuses() []Status
	EarliestReset() QuotaBudget
	RefreshQuota(ctx context.Context, id string) (Sample, string, error)
}

// RefreshQuota queries one source (or every source when id is empty) and applies
// the reading. The raw payload is returned so /quota can show what the vendor
// actually sent when field mapping drifts.
func (p *Pool) RefreshQuota(ctx context.Context, id string) (Sample, string, error) {
	if id == "" {
		var firstErr error
		for _, provider := range p.Providers() {
			// Disabled sources (e.g. no API key yet) must not generate probe
			// traffic or auth errors on every refresh.
			if !provider.Enabled() {
				continue
			}
			if _, _, err := p.refreshProvider(ctx, provider); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		_ = p.SaveQuotaState()
		return Sample{}, "", firstErr
	}
	provider, err := p.byID(id)
	if err != nil {
		return Sample{}, "", err
	}
	sample, raw, err := p.refreshProvider(ctx, provider)
	_ = p.SaveQuotaState()
	return sample, raw, err
}

func (p *Pool) refreshProvider(ctx context.Context, provider *Provider) (Sample, string, error) {
	probe := newProbe(provider.cfg)
	atomic.StoreInt64(&provider.lastProbeUnix, p.now().Unix())
	sample, raw, err := probe.Query(ctx, provider.cfg)
	if err != nil {
		p.recordFailure(provider.ID(), FailureOther, fmt.Errorf("余量探针：%w", err))
		return sample, raw, err
	}
	sample = convertSample(sample, provider.cfg)
	provider.quota.observePlan(sample.Plan)
	provider.quota.applyProbe(sample, p.now(), sample.Unit)
	p.mu.Lock()
	delete(p.lastErr, provider.ID())
	p.mu.Unlock()
	return sample, raw, nil
}

// convertSample folds a percentage reading into the channel's own unit, so a
// plan expressed in credits and a probe that only reports "% used" still speak
// the same language (and the self-imposed cap keeps working).
func convertSample(sample Sample, cfg config.ProviderConfig) Sample {
	if !strings.EqualFold(cfg.Quota.Probe.Unit, "percent") {
		return sample
	}
	if sample.HasUsed {
		sample.Used = math.Min(100, math.Max(0, sample.Used))
	}
	if !sample.HasTotal {
		sample.Total, sample.HasTotal = 100, true
	}
	capacity := cfg.Quota.Local.Total
	unit := cfg.Quota.Local.Unit
	if capacity <= 0 {
		// No configured capacity: keep the raw percentage scale.
		sample.Unit = "percent"
		return sample
	}
	sample.Used = sample.Used / sample.Total * capacity
	sample.Total = capacity
	sample.Unit = unit
	return sample
}

// StartQuotaLoop polls every source on its own interval until ctx is done. Probe
// failures never stop work: they only leave local accounting in charge.
func (p *Pool) StartQuotaLoop(ctx context.Context) {
	interval := p.pollInterval()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = p.SaveQuotaState()
			return
		case <-ticker.C:
			for _, provider := range p.Providers() {
				if !provider.Enabled() || !provider.probeDue(interval, p.now()) {
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, _, _ = p.refreshProvider(probeCtx, provider)
				cancel()
			}
			_ = p.SaveQuotaState()
		}
	}
}

// pollInterval is the shortest configured probe interval, floored at 30s so the
// tool itself never looks like abusive traffic to the vendor.
func (p *Pool) pollInterval() time.Duration {
	shortest := time.Duration(0)
	for _, provider := range p.Providers() {
		candidate := provider.cfg.Quota.Probe.Interval()
		if shortest == 0 || candidate < shortest {
			shortest = candidate
		}
	}
	if shortest < 30*time.Second {
		return 30 * time.Second
	}
	return shortest
}

func (p *Provider) probeDue(interval time.Duration, now time.Time) bool {
	last := time.Unix(atomic.LoadInt64(&p.lastProbeUnix), 0)
	want := p.cfg.Quota.Probe.Interval()
	if want <= 0 {
		want = interval
	}
	return last.IsZero() || now.Sub(last) >= want
}

// QuotaLines renders a compact, stable summary for the TUI status line.
func (p *Pool) QuotaLines() []string {
	now := p.now()
	var lines []string
	for _, status := range p.Statuses() {
		marker := "  "
		if status.Active {
			marker = "▸ "
		}
		state := "可用"
		switch {
		case !status.Enabled:
			state = "已停用"
		case status.Exhausted:
			state = "已耗尽"
		case status.Capped:
			state = "已达自设上限"
		case now.Before(status.CooldownTill):
			state = "冷却中"
		}
		ratio := "未知"
		if status.Total > 0 {
			ratio = fmt.Sprintf("%.0f%% 余量", status.Ratio*100)
			if status.SelfLimited {
				ratio += fmt.Sprintf("（自设上限 %.0f%%，可用 %s）", status.MaxUsageRatio*100, FormatUnits(status.Usable))
			}
		}
		reset := "无窗口"
		if !status.ResetAt.IsZero() {
			reset = "⟶" + status.ResetAt.Local().Format("15:04")
		}
		channel := status.Vendor
		if status.Plan != "" {
			channel += "/" + status.Plan
		}
		if status.PlanObserved != "" && planDiffers(status.Plan, status.PlanObserved) {
			channel += "→ 接口报告 " + status.PlanObserved + "，请核对 plan"
		}
		reason := ""
		if !status.Enabled && status.DisabledReason != "" {
			reason = "（" + status.DisabledReason + "）"
		}
		lines = append(lines, fmt.Sprintf("%s%s（%s · %s）%s · %s · %s · %s · %s · 并发 %d/%d",
			marker, status.Label, status.ID, channel, ratio, state+reason, reset, status.Unit, status.Model, status.Concurrency, status.MaxConcur))
	}
	return lines
}

// QuotaSummary is one line for the header: active source and its remaining 5h
// quota, which is the number a user actually watches.
func (p *Pool) QuotaSummary() string {
	now := p.now()
	statuses := p.Statuses()
	if len(statuses) == 0 {
		return "无源"
	}
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if !status.Enabled {
			continue
		}
		value := "?"
		if status.Total > 0 {
			value = fmt.Sprintf("%.0f%%", status.Ratio*100)
		}
		label := status.ID
		if status.Active {
			label = "*" + label
		}
		if status.Exhausted || now.Before(status.CooldownTill) || status.Capped {
			value = "空"
		}
		if status.MaxConcur > 0 && status.Concurrency < status.MaxConcur {
			// Show throttling in the header: it explains slowness better than any
			// other single number.
			value += fmt.Sprintf("·并发%d/%d", status.Concurrency, status.MaxConcur)
		}
		parts = append(parts, label+" "+value)
	}
	return strings.Join(parts, " | ")
}

// planDiffers compares a configured plan id with the vendor's tier name, which
// is the bare tier ("pro") while the plan id is prefixed ("glm-pro").
func planDiffers(configured, observed string) bool {
	configured = strings.ToLower(strings.TrimSpace(configured))
	observed = strings.ToLower(strings.TrimSpace(observed))
	if configured == "" || observed == "" {
		return false
	}
	if configured == observed {
		return false
	}
	return !strings.HasSuffix(configured, "-"+observed)
}

// FormatUnits renders a quota amount without scientific notation: credits are
// meant to be read by a human, and 1.08e+04 is not.
func FormatUnits(value float64) string {
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return strconv.FormatInt(int64(value), 10)
	}
	if math.Abs(value) >= 100 {
		return fmt.Sprintf("%.0f", value)
	}
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}

// ProbeConfigOf exposes a source's probe configuration for diagnostics.
func (p *Pool) ProbeConfigOf(id string) (config.ProbeConfig, bool) {
	provider := p.find(id)
	if provider == nil {
		return config.ProbeConfig{}, false
	}
	return provider.cfg.Quota.Probe, true
}

// ListModels asks the channel which model names it accepts. It is the quickest
// way to fix a "model not found" error: the endpoint itself reports the names.
func (p *Pool) ListModels(ctx context.Context, id string) ([]string, error) {
	provider, err := p.byID(id)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(provider.cfg.OpenAI.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("渠道 %s 没有 base_url", id)
	}
	timeout := time.Duration(provider.cfg.OpenAI.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+provider.cfg.OpenAI.APIKey)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s/models 返回 HTTP %d（%s）", base, resp.StatusCode, snippet(string(body), 160))
	}
	return parseModelList(body)
}

func parseModelList(body []byte) ([]string, error) {
	var envelope struct {
		Data   []struct{ ID string `json:"id"` } `json:"data"`
		Models []string                          `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		// Some gateways answer with a bare array of names or objects.
		var list []string
		if jsonErr := json.Unmarshal(body, &list); jsonErr == nil {
			return list, nil
		}
		var objects []struct{ ID string `json:"id"` }
		if jsonErr := json.Unmarshal(body, &objects); jsonErr == nil {
			for _, object := range objects {
				list = append(list, object.ID)
			}
			return list, nil
		}
		return nil, fmt.Errorf("无法解析模型列表：%s", snippet(string(body), 160))
	}
	var names []string
	for _, entry := range envelope.Data {
		if entry.ID != "" {
			names = append(names, entry.ID)
		}
	}
	names = append(names, envelope.Models...)
	if len(names) == 0 {
		return nil, fmt.Errorf("端点返回了空的模型列表：%s", snippet(string(body), 160))
	}
	return names, nil
}
