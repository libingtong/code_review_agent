package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Workspace      string       `yaml:"workspace"`
	OpenAI         OpenAIConfig `yaml:"openai"`
	CompressOpenAI OpenAIConfig `yaml:"compress_openai"`
	Prompts        PromptConfig `yaml:"prompts"`
	Agent          AgentConfig  `yaml:"agent"`

	// Multi-source extensions. With no providers configured the loader
	// synthesises a single source from OpenAI, so existing files keep working.
	Providers    []ProviderConfig   `yaml:"providers"`
	Quota        QuotaConfig        `yaml:"quota"`
	Routing      RoutingConfig      `yaml:"routing"`
	Concurrency  ConcurrencyConfig  `yaml:"concurrency"`
	Schedule     ScheduleConfig     `yaml:"schedule"`
	Orchestrator OrchestratorConfig `yaml:"orchestrator"`
	Logging      LoggingConfig      `yaml:"logging"`
}

// MaxTeamAgents is the single upper bound shared by config validation and
// session restore. Recon and audit sizes must agree on it, otherwise a session
// can be saved and then rejected on reload.
const MaxTeamAgents = 666

// MinCompressionSource is the tightest (context, output) pair across every
// enabled source. Multi-source runs must compress for the smallest window or a
// request that fits one provider is rejected by another.
type MinCompressionSource struct {
	Context int
	Output  int
	Source  string
}

type OpenAIConfig struct {
	BaseURL          string          `yaml:"base_url"`
	APIInterface     string          `yaml:"api_interface"`
	APIKey           string          `yaml:"api_key"`
	APIKeyEnv        string          `yaml:"api_key_env"`
	Model            string          `yaml:"model"`
	Temperature      float64         `yaml:"temperature"`
	TopP             float64         `yaml:"top_p"`
	MaxContextTokens int             `yaml:"max_context_tokens"`
	MaxOutputTokens  int             `yaml:"max_output_tokens"`
	TimeoutSeconds   int             `yaml:"timeout_seconds"`
	Stream           bool            `yaml:"stream"`
	Thinking         *ThinkingConfig `yaml:"thinking"`
	ReasoningEffort  string          `yaml:"reasoning_effort"`
	// History selects how tool traffic is replayed on the Responses protocol:
	// "auto" (native items, degrade on rejection), "native" (always native) or
	// "text" (always plain text). GLM's Responses endpoint rejects native replays
	// often enough that "text" is the reliable setting for it.
	History string `yaml:"history"`
}

// HistoryMode normalises the history replay setting.
func (c OpenAIConfig) HistoryMode() string {
	switch strings.ToLower(strings.TrimSpace(c.History)) {
	case "text", "plain":
		return "text"
	case "native":
		return "native"
	default:
		return "auto"
	}
}

func validateHistory(cfg OpenAIConfig, section string) error {
	switch cfg.HistoryMode() {
	case "auto", "native", "text":
		return nil
	}
	return fmt.Errorf("%s.history must be auto, native or text", section)
}

type ThinkingConfig struct {
	Type string `yaml:"type" json:"type"`
}

func validateReasoning(cfg OpenAIConfig, section string) error {
	if cfg.Thinking != nil && cfg.Thinking.Type != "enabled" && cfg.Thinking.Type != "adaptive" {
		return fmt.Errorf("%s.thinking.type must be enabled or adaptive; disabled is not supported", section)
	}
	switch cfg.ReasoningEffort {
	case "", "minimal", "low", "medium", "high", "max":
		return nil
	default:
		return fmt.Errorf("%s.reasoning_effort must be minimal, low, medium, high or max", section)
	}
}

type PromptConfig struct {
	System       string `yaml:"system"`
	PlanSystem   string `yaml:"plan_system"`
	Compress     string `yaml:"compress"`
	SkillsDir    string `yaml:"skills_dir"`
	TemplatesDir string `yaml:"templates_dir"`
}

type AgentConfig struct {
	MaxTurns             int     `yaml:"max_turns"`
	SessionDir           string  `yaml:"session_dir"`
	LogSession           bool    `yaml:"log_session"`
	LogSessionDir        string  `yaml:"log_session_dir"`
	RetryAttempts        int     `yaml:"retry_attempts"`
	CompressAtRatio      float64 `yaml:"compress_at_ratio"`
	CompressBufferTokens int     `yaml:"compress_buffer_tokens"`
	AutoPlan             bool    `yaml:"auto_plan"`
	MaxToolResultChars   int     `yaml:"max_tool_result_chars"`
	MaxToolsPerTurn      int     `yaml:"max_tools_per_turn"`
	MaxProtocolFailures  int     `yaml:"max_protocol_failures"`
	// ModeratorIdleMinutes is the heartbeat for a moderator review when neither
	// the forum nor the stage changed; ticks in between are skipped.
	ModeratorIdleMinutes     int   `yaml:"moderator_idle_minutes"`
	ReconAgents              int   `yaml:"recon_agents"`
	AuditAgents              int   `yaml:"audit_agents"`
	ForumWaitSeconds         int   `yaml:"forum_wait_seconds"`
	ModeratorEnabled         *bool `yaml:"moderator_enabled"`
	ModeratorIntervalSeconds int   `yaml:"moderator_interval_seconds"`
	InfiniteMode             bool  `yaml:"infinite_mode"`
	BudgetHours              int   `yaml:"budget_hours"`
	BudgetMinutes            int   `yaml:"budget_minutes"`
	BudgetTokens             int64 `yaml:"budget_tokens"`
}

// BudgetDuration keeps explicit zero as unlimited and rejects overflow before
// converting user-supplied hours/minutes to a timer duration.
func (cfg AgentConfig) BudgetDuration() (time.Duration, error) {
	const maxMinutes = int64(math.MaxInt64) / int64(time.Minute)
	if cfg.BudgetHours < 0 || cfg.BudgetMinutes < 0 || cfg.BudgetTokens < 0 {
		return 0, fmt.Errorf("audit budgets must be nonnegative; 0 means unlimited")
	}
	if int64(cfg.BudgetMinutes) > maxMinutes || int64(cfg.BudgetHours) > (maxMinutes-int64(cfg.BudgetMinutes))/60 {
		return 0, fmt.Errorf("audit time budget overflows time.Duration")
	}
	return time.Duration(int64(cfg.BudgetHours)*60+int64(cfg.BudgetMinutes)) * time.Minute, nil
}

func Load(path string) (Config, error) {
	// Make a directly-invoked binary behave like run.sh: keys usually live in .env.
	loadDotEnvNear(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	// Seed required defaults before decoding so explicit zero remains invalid.
	cfg := Config{
		OpenAI: OpenAIConfig{MaxContextTokens: 32000, MaxOutputTokens: 4096},
		Agent:  AgentConfig{CompressAtRatio: 0.75, BudgetHours: 8},
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Config{}, err
	}
	if err := document.Decode(&cfg); err != nil {
		return Config{}, err
	}
	// Missing compressor limits inherit from the main model; explicit zero is
	// an invalid limit, not an instruction to silently substitute a default.
	if len(document.Content) > 0 {
		root := document.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "compress_openai" {
				continue
			}
			section := root.Content[i+1]
			for j := 0; j+1 < len(section.Content); j += 2 {
				key := section.Content[j].Value
				if (key == "max_context_tokens" && cfg.CompressOpenAI.MaxContextTokens <= 0) || (key == "max_output_tokens" && cfg.CompressOpenAI.MaxOutputTokens <= 0) {
					return Config{}, fmt.Errorf("compress_openai.%s must be positive", key)
				}
			}
		}
	}
	applyDefaults(&cfg)
	cfg.Logging.applyDefaults()
	if err := cfg.Logging.Validate(); err != nil {
		return Config{}, err
	}
	if len(cfg.Providers) == 0 {
		if err := validateReasoning(cfg.OpenAI, "openai"); err != nil {
			return Config{}, err
		}
	}
	if err := validateProviders(&cfg); err != nil {
		return Config{}, err
	}
	// Compression budgets are derived from the normalised channels, so this must
	// run after presets and explicit limits are resolved.
	applyCompressDefaults(&cfg)
	if err := validateReasoning(cfg.CompressOpenAI, "compress_openai"); err != nil {
		return Config{}, err
	}
	if _, err := cfg.Agent.BudgetDuration(); err != nil {
		return Config{}, err
	}
	if cfg.Agent.ReconAgents < 1 || cfg.Agent.ReconAgents > MaxTeamAgents || cfg.Agent.AuditAgents < 1 || cfg.Agent.AuditAgents > MaxTeamAgents {
		return Config{}, fmt.Errorf("agent.recon_agents and agent.audit_agents must be between 1 and %d", MaxTeamAgents)
	}
	if cfg.Agent.ModeratorIntervalSeconds < 1 || cfg.Agent.ModeratorIntervalSeconds > 86400 {
		return Config{}, fmt.Errorf("agent.moderator_interval_seconds must be between 1 and 86400")
	}
	if cfg.Agent.ForumWaitSeconds < 1 || cfg.Agent.ForumWaitSeconds > 120 {
		return Config{}, fmt.Errorf("agent.forum_wait_seconds must be between 1 and 120")
	}
	if cfg.Agent.ModeratorIdleMinutes < 1 || cfg.Agent.ModeratorIdleMinutes > 240 {
		return Config{}, fmt.Errorf("agent.moderator_idle_minutes must be between 1 and 240")
	}
	if cfg.Agent.MaxProtocolFailures < 1 || cfg.Agent.MaxProtocolFailures > 20 {
		return Config{}, fmt.Errorf("agent.max_protocol_failures must be between 1 and 20")
	}
	if cfg.Agent.MaxToolsPerTurn < 1 || cfg.Agent.MaxToolsPerTurn > 8 {
		return Config{}, fmt.Errorf("agent.max_tools_per_turn must be between 1 and 8")
	}
	if cfg.Agent.MaxToolResultChars < 1024 || cfg.Agent.MaxToolResultChars > 65536 {
		return Config{}, fmt.Errorf("agent.max_tool_result_chars must be between 1024 and 65536 (serialized UTF-8 bytes)")
	}
	if _, err := cfg.CompressionThreshold(); err != nil {
		return Config{}, err
	}
	if _, err := cfg.CompressionInputBudget(); err != nil {
		return Config{}, err
	}
	if len(cfg.Providers) == 0 {
		resolveAPIKey(&cfg.OpenAI)
	}
	resolveAPIKey(&cfg.CompressOpenAI)
	return cfg, nil
}

// CompressionThreshold is an estimated-token admission limit, not tokenizer usage.
// Tool results are bounded UTF-8 bytes; reserve one token per byte conservatively,
// plus the notification allowance and message framing, independently of the ratio.
func (cfg Config) CompressionThreshold() (int, error) {
	limits, err := cfg.EffectiveTokenLimits()
	if err != nil {
		return 0, err
	}
	context, output := limits.Context, limits.Output
	ratio := cfg.Agent.CompressAtRatio
	if context <= 0 || output <= 0 || output >= context {
		return 0, fmt.Errorf("openai token limits must be positive with output below context")
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio >= 1 {
		return 0, fmt.Errorf("agent.compress_at_ratio must be finite and strictly between 0 and 1")
	}
	if cfg.Agent.CompressBufferTokens < 0 || cfg.Agent.MaxToolResultChars < 0 || cfg.Agent.MaxToolResultChars > 65536 {
		return 0, fmt.Errorf("compression reserve and tool result bounds are invalid")
	}
	toolBytes := cfg.Agent.MaxToolResultChars
	if toolBytes < 1024 {
		toolBytes = 1024
	}
	reserve := toolBytes + 2048 + 256
	if cfg.Agent.CompressBufferTokens > reserve {
		reserve = cfg.Agent.CompressBufferTokens
	}
	if reserve >= context-output {
		return 0, fmt.Errorf("openai context cannot fit output plus compression/tool/notification reserve")
	}
	limit := context - output - reserve
	ratioLimit := math.Floor(float64(context) * ratio)
	if ratioLimit < float64(limit) {
		limit = int(ratioLimit)
	}
	if limit < 1024 {
		return 0, fmt.Errorf("effective compression threshold must leave at least 1024 estimated input tokens")
	}
	return limit, nil
}

// CompressionInputBudget reserves the configured summarizer output in full.
func (cfg Config) CompressionInputBudget() (int, error) {
	context, output := cfg.CompressOpenAI.MaxContextTokens, cfg.CompressOpenAI.MaxOutputTokens
	if context <= 0 || output <= 0 || output >= context {
		return 0, fmt.Errorf("compress_openai token limits must be positive with output below context")
	}
	return context - output, nil
}

func resolveAPIKey(cfg *OpenAIConfig) {
	if cfg.APIKey != "" || cfg.APIKeyEnv == "" {
		return
	}
	if looksLikeAPIKey(cfg.APIKeyEnv) {
		cfg.APIKey = cfg.APIKeyEnv
	} else {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
	}
}

func looksLikeAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "sk-") {
		return true
	}
	if strings.Contains(value, "-") && len(value) >= 20 {
		return true
	}
	return false
}

func applyDefaults(cfg *Config) {
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	// The global openai section only survives as the single-source legacy path:
	// with providers configured, every channel owns its own settings and nothing
	// is inherited.
	if len(cfg.Providers) == 0 {
		applyLegacyOpenAIDefaults(cfg)
	}
	if cfg.Prompts.System == "" {
		cfg.Prompts.System = "prompts/system.md"
	}
	if cfg.Prompts.PlanSystem == "" {
		cfg.Prompts.PlanSystem = "prompts/plan_system.md"
	}
	if cfg.Prompts.Compress == "" {
		cfg.Prompts.Compress = "prompts/compress.md"
	}
	if cfg.Prompts.SkillsDir == "" {
		cfg.Prompts.SkillsDir = "skills"
	}
	if cfg.Prompts.TemplatesDir == "" {
		cfg.Prompts.TemplatesDir = "prompts/templates"
	}
	if cfg.Agent.SessionDir == "" {
		cfg.Agent.SessionDir = "sessions"
	}
	if cfg.Agent.LogSessionDir == "" {
		cfg.Agent.LogSessionDir = "log_sessions"
	}
	if cfg.Agent.RetryAttempts == 0 {
		cfg.Agent.RetryAttempts = 3
	}
	if cfg.Agent.CompressBufferTokens == 0 {
		source := cfg.OpenAI
		if len(cfg.Providers) > 0 {
			source = tightestProvider(cfg.Providers)
		}
		// Reserve must fit the tightest channel: half of the headroom left after
		// the output budget, so a 64k-context channel still configures cleanly.
		buffer := source.MaxOutputTokens
		if headroom := (source.MaxContextTokens - source.MaxOutputTokens) / 2; headroom < buffer {
			buffer = headroom
		}
		if buffer < 4096 {
			buffer = 4096
		}
		cfg.Agent.CompressBufferTokens = buffer
	}
	if cfg.Agent.MaxToolResultChars == 0 {
		cfg.Agent.MaxToolResultChars = 12000
	}
	if cfg.Agent.ModeratorIdleMinutes == 0 {
		cfg.Agent.ModeratorIdleMinutes = 10
	}
	if cfg.Agent.MaxProtocolFailures == 0 {
		cfg.Agent.MaxProtocolFailures = 3
	}
	if cfg.Agent.MaxToolsPerTurn == 0 {
		// Conservative default: one tool per turn, exactly the old behaviour.
		// Raise to 2-3 to trade some speculative calls for throughput.
		cfg.Agent.MaxToolsPerTurn = 1
	}
	if cfg.Agent.ReconAgents == 0 {
		cfg.Agent.ReconAgents = 4
	}
	if cfg.Agent.AuditAgents == 0 {
		cfg.Agent.AuditAgents = 4
	}
	if cfg.Agent.ModeratorIntervalSeconds == 0 {
		cfg.Agent.ModeratorIntervalSeconds = 60
	}
	if cfg.Agent.ForumWaitSeconds == 0 {
		cfg.Agent.ForumWaitSeconds = 60
	}
}

func (cfg AgentConfig) ModeratorIsEnabled() bool {
	return cfg.ModeratorEnabled == nil || *cfg.ModeratorEnabled
}

// applyLegacyOpenAIDefaults keeps a providers-less config working exactly like
// the single-source release did.
func applyLegacyOpenAIDefaults(cfg *Config) {
	if cfg.OpenAI.Thinking == nil {
		cfg.OpenAI.Thinking = &ThinkingConfig{Type: "enabled"}
	}
	if cfg.OpenAI.ReasoningEffort == "" {
		cfg.OpenAI.ReasoningEffort = "high"
	}
	if cfg.OpenAI.BaseURL == "" {
		cfg.OpenAI.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.OpenAI.APIInterface == "" {
		cfg.OpenAI.APIInterface = "responses"
	}
	if cfg.OpenAI.Model == "" {
		cfg.OpenAI.Model = "gpt-4o-mini"
	}
	if cfg.OpenAI.TopP == 0 {
		cfg.OpenAI.TopP = 1
	}
	if cfg.OpenAI.TimeoutSeconds == 0 {
		cfg.OpenAI.TimeoutSeconds = 10
	}
}

// applyCompressDefaults derives the compressor budget from the tightest channel
// when compress_openai is omitted. With a provider pool the model/endpoint are
// never used for compression (the pool serves it), only the token budget is.
// applyCompressDefaults fills the compressor budget from the tightest channel.
// Explicit compress_openai values always win; the endpoint/model are unused when
// a provider pool serves compression, only the token budget matters.
func applyCompressDefaults(cfg *Config) {
	source := cfg.OpenAI
	if len(cfg.Providers) > 0 {
		source = tightestProvider(cfg.Providers)
	}
	c := cfg.CompressOpenAI
	if c.BaseURL == "" {
		c.BaseURL = source.BaseURL
	}
	if c.APIInterface == "" {
		c.APIInterface = source.APIInterface
	}
	if c.APIKey == "" && c.APIKeyEnv == "" {
		c.APIKey, c.APIKeyEnv = source.APIKey, source.APIKeyEnv
	}
	if c.Model == "" {
		c.Model = source.Model
	}
	if c.Thinking == nil {
		c.Thinking = &ThinkingConfig{Type: "enabled"}
	}
	if c.ReasoningEffort == "" {
		c.ReasoningEffort = "high"
	}
	if c.Temperature == 0 {
		c.Temperature = source.Temperature
	}
	if c.TopP == 0 {
		c.TopP = source.TopP
	}
	if c.MaxContextTokens == 0 {
		c.MaxContextTokens = source.MaxContextTokens
	}
	if c.MaxOutputTokens == 0 {
		c.MaxOutputTokens = source.MaxOutputTokens
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = source.TimeoutSeconds
	}
	c.Stream = false
	cfg.CompressOpenAI = c
}

func tightestProvider(providers []ProviderConfig) OpenAIConfig {
	var best OpenAIConfig
	for _, provider := range providers {
		if !provider.IsEnabled() || provider.OpenAI.MaxContextTokens <= 0 {
			continue
		}
		if best.MaxContextTokens == 0 || provider.OpenAI.MaxContextTokens < best.MaxContextTokens {
			best = provider.OpenAI
		}
	}
	return best
}
