package config

import (
	"os"
	"path/filepath"
	"strings"
)

// loadDotEnv reads a .env file (KEY=VALUE per line) and fills only variables that
// are not already present in the environment. run.sh sources the same file, but
// running the binary directly (or from an IDE) must not silently lose the keys:
// "missing API key" on every agent is a confusing way to learn that.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		// Strip one layer of matching quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

// loadDotEnvNear looks for .env beside the config file, then in the working
// directory, so both `-config path/to/config.yaml` and plain invocation work.
func loadDotEnvNear(configPath string) {
	seen := map[string]bool{}
	candidates := []string{
		filepath.Join(filepath.Dir(configPath), ".env"),
		".env",
	}
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate)
		if err != nil || seen[absolute] {
			continue
		}
		seen[absolute] = true
		loadDotEnv(absolute)
	}
}

// MissingKeys lists enabled channels whose API key could not be resolved.
func (cfg Config) MissingKeys() []string {
	var missing []string
	for _, provider := range cfg.Providers {
		if !provider.IsEnabled() || strings.TrimSpace(provider.OpenAI.APIKey) != "" {
			continue
		}
		hint := provider.OpenAI.APIKeyEnv
		if hint == "" {
			hint = "openai.api_key"
		}
		missing = append(missing, provider.ID+"（"+hint+"）")
	}
	return missing
}
