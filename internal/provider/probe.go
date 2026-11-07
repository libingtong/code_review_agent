package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"code-review-agent/internal/config"
)

// Sample is one vendor quota reading. Every field is optional because vendors
// expose different (and sometimes partial) information.
type Sample struct {
	Used     float64
	Total    float64
	ResetAt  time.Time
	Unit     string
	Plan     string
	HasUsed  bool
	HasTotal bool
	HasReset bool
}

// Probe queries one vendor. The raw payload is returned for /quota so a user can
// re-map field paths without rebuilding the program.
type Probe interface {
	Kind() string
	Query(ctx context.Context, cfg config.ProviderConfig) (Sample, string, error)
}

func newProbe(cfg config.ProviderConfig) Probe {
	switch strings.ToLower(strings.TrimSpace(cfg.Quota.Probe.Type)) {
	case "http":
		return &httpProbe{}
	case "command":
		return &commandProbe{}
	case "local", "auto":
		return &localProbe{}
	case "":
		if cfg.Quota.Probe.URL != "" {
			return &httpProbe{}
		}
		if len(cfg.Quota.Probe.Command) > 0 {
			return &commandProbe{}
		}
		return &localProbe{}
	}
	return &localProbe{}
}

type httpProbe struct{}

func (h *httpProbe) Kind() string { return "http" }

func (h *httpProbe) Query(ctx context.Context, cfg config.ProviderConfig) (Sample, string, error) {
	timeout := time.Duration(cfg.Quota.Probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := strings.ToUpper(strings.TrimSpace(cfg.Quota.Probe.Method))
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.Quota.Probe.URL, nil)
	if err != nil {
		return Sample{}, "", err
	}
	for key, value := range cfg.Quota.Probe.Headers {
		req.Header.Set(key, strings.ReplaceAll(value, "${api_key}", cfg.OpenAI.APIKey))
	}
	if req.Header.Get("Authorization") == "" && cfg.OpenAI.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.OpenAI.APIKey)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return Sample{}, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Sample{}, "", err
	}
	raw := strings.TrimSpace(string(body))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Sample{}, raw, fmt.Errorf("探针返回 HTTP %d", resp.StatusCode)
	}
	sample, err := sampleFromJSON(body, cfg.Quota.Probe.Bind, time.Now())
	if err != nil {
		// Vendors often answer HTTP 200 with an error envelope. Surface that
		// rather than a confusing "no bind path matched".
		if msg := vendorError(body); msg != "" {
			return sample, raw, fmt.Errorf("探针返回业务错误：%s", msg)
		}
		// Include a body snippet: without it a field rename is invisible and the
		// user cannot fix bind paths from /quota.
		return sample, raw, fmt.Errorf("%w（响应片段：%s）", err, snippet(raw, 240))
	}
	return sample, raw, err
}

type commandProbe struct{}

func (c *commandProbe) Kind() string { return "command" }

func (c *commandProbe) Query(ctx context.Context, cfg config.ProviderConfig) (Sample, string, error) {
	timeout := time.Duration(cfg.Quota.Probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := make([]string, len(cfg.Quota.Probe.Command))
	for i, arg := range cfg.Quota.Probe.Command {
		args[i] = strings.ReplaceAll(arg, "${api_key}", cfg.OpenAI.APIKey)
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := command.Output()
	raw := strings.TrimSpace(string(output))
	if err != nil {
		return Sample{}, raw, fmt.Errorf("探针命令失败：%w", err)
	}
	now := time.Now()
	var asJSON any
	if json.Unmarshal(output, &asJSON) == nil {
		if sample, jsonErr := sampleFromValue(asJSON, cfg.Quota.Probe.Bind, now); jsonErr == nil {
			return sample, raw, nil
		}
	}
	sample, regexErr := sampleFromRegex(raw, cfg.Quota.Probe.Regex, now)
	return sample, raw, regexErr
}

type localProbe struct{}

func (l *localProbe) Kind() string { return "local" }

// Query reports that no external reading is available; the window keeps its
// local accounting.
func (l *localProbe) Query(ctx context.Context, cfg config.ProviderConfig) (Sample, string, error) {
	return Sample{}, "", fmt.Errorf("该源仅使用本地计量")
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func sampleFromJSON(body []byte, bind config.BindConfig, now time.Time) (Sample, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return Sample{}, fmt.Errorf("探针响应不是 JSON：%w", err)
	}
	return sampleFromValue(root, bind, now)
}

func sampleFromValue(root any, bind config.BindConfig, now time.Time) (Sample, error) {
	var sample Sample
	for _, path := range bind.Used {
		if value, ok := lookupValue(root, path); ok {
			if number, ok := asFloat(value); ok {
				sample.Used, sample.HasUsed = number, true
				break
			}
		}
	}
	for _, path := range bind.Total {
		if value, ok := lookupValue(root, path); ok {
			if number, ok := asFloat(value); ok {
				sample.Total, sample.HasTotal = number, true
				break
			}
		}
	}
	for _, path := range bind.Plan {
		if value, ok := lookupPath(root, path); ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				sample.Plan = strings.ToLower(strings.TrimSpace(text))
				break
			}
		}
	}
	for _, path := range bind.Reset {
		if value, ok := lookupValue(root, path); ok {
			if at, ok := asTime(value, now); ok {
				sample.ResetAt, sample.HasReset = at, true
				break
			}
		}
	}
	if !sample.HasUsed && !sample.HasTotal && !sample.HasReset {
		return Sample{}, fmt.Errorf("探针响应未命中任何 bind 路径")
	}
	return sample, nil
}

var percentRegex = regexp.MustCompile(`(?i)(used|已用|消耗)[^0-9%]{0,16}([0-9]+(?:\.[0-9]+)?)\s*%`)

func sampleFromRegex(text string, pattern string, now time.Time) (Sample, error) {
	if strings.TrimSpace(pattern) != "" {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return Sample{}, fmt.Errorf("探针正则无效：%w", err)
		}
		groups := compiled.FindStringSubmatch(text)
		if groups == nil {
			return Sample{}, fmt.Errorf("探针输出未匹配正则")
		}
		names := compiled.SubexpNames()
		sample := Sample{Total: 100, Unit: "%"}
		for i := 1; i < len(groups); i++ {
			if i >= len(names) {
				break
			}
			switch names[i] {
			case "used":
				if number, err := strconv.ParseFloat(groups[i], 64); err == nil {
					sample.Used, sample.HasUsed = number, true
				}
			case "total":
				if number, err := strconv.ParseFloat(groups[i], 64); err == nil {
					sample.Total, sample.HasTotal = number, true
				}
			case "reset":
				if at, ok := asTime(groups[i], now); ok {
					sample.ResetAt, sample.HasReset = at, true
				}
			}
		}
		if !sample.HasUsed {
			return Sample{}, fmt.Errorf("探针正则缺少 used 命名分组")
		}
		return sample, nil
	}
	match := percentRegex.FindStringSubmatch(text)
	if match == nil {
		return Sample{}, fmt.Errorf("探针输出中找不到百分比用量")
	}
	number, err := strconv.ParseFloat(match[2], 64)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Used: number, Total: 100, Unit: "%", HasUsed: true, HasTotal: true}, nil
}

// lookupValue resolves a bind entry, which may also be a literal constant such
// as "=100": percentage-only payloads still need a comparable total.
func lookupValue(root any, path string) (any, bool) {
	if strings.HasPrefix(path, "=") {
		return strings.TrimPrefix(path, "="), true
	}
	return lookupPath(root, path)
}

// lookupPath resolves a tiny path language: dotted keys, [n] indexes and
// [field=value] filters. It exists so users can retarget vendor fields from YAML.
func lookupPath(root any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	current := root
	for len(path) > 0 {
		switch {
		case strings.HasPrefix(path, "."):
			path = path[1:]
		case strings.HasPrefix(path, "["):
			end := strings.Index(path, "]")
			if end < 0 {
				return nil, false
			}
			selector := path[1:end]
			path = path[end+1:]
			switch {
			case strings.Contains(selector, "="):
				parts := strings.SplitN(selector, "=", 2)
				key, want := strings.TrimSpace(parts[0]), strings.Trim(strings.TrimSpace(parts[1]), `"'`)
				list, ok := current.([]any)
				if !ok {
					return nil, false
				}
				found := false
				for _, item := range list {
					if object, ok := item.(map[string]any); ok {
						if fmt.Sprint(object[key]) == want {
							current, found = item, true
							break
						}
					}
				}
				if !found {
					return nil, false
				}
			default:
				index, err := strconv.Atoi(strings.TrimSpace(selector))
				if err != nil {
					return nil, false
				}
				list, ok := current.([]any)
				if !ok || index < 0 || index >= len(list) {
					return nil, false
				}
				current = list[index]
			}
		default:
			end := len(path)
			for i, r := range path {
				if r == '.' || r == '[' {
					end = i
					break
				}
			}
			key := path[:end]
			path = path[end:]
			object, ok := current.(map[string]any)
			if !ok {
				return nil, false
			}
			value, exists := object[key]
			if !exists {
				return nil, false
			}
			current = value
		}
	}
	return current, true
}

// vendorError extracts a non-success code/message from a 200 response body.
func vendorError(body []byte) string {
	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"msg"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ""
	}
	message := strings.TrimSpace(firstNonEmpty(envelope.Message, envelope.Error.Message))
	if message == "" {
		return ""
	}
	code, ok := asFloat(envelope.Code)
	if !ok || code == 0 || code == 200 {
		return ""
	}
	return fmt.Sprintf("code=%v %s", envelope.Code, message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func snippet(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil
	case string:
		trimmed := strings.TrimSuffix(strings.TrimSpace(typed), "%")
		number, err := strconv.ParseFloat(trimmed, 64)
		return number, err == nil
	}
	return 0, false
}

func asTime(value any, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case float64:
		seconds := int64(typed)
		if seconds > 1_000_000_000_000 {
			seconds /= 1000
		}
		return time.Unix(seconds, 0), seconds > 0
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			if number > 1_000_000_000_000 {
				number /= 1000
			}
			return time.Unix(number, 0), number > 0
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339Nano} {
			if at, err := time.Parse(layout, text); err == nil {
				return at, true
			}
		}
		if duration, err := time.ParseDuration(text); err == nil {
			return now.Add(duration), true
		}
	}
	return time.Time{}, false
}
