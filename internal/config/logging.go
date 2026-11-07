package config

import (
	"fmt"
	"strings"
)

// LoggingConfig controls the diagnostic log. It is off by default: a TUI program
// should not write files unless asked.
type LoggingConfig struct {
	Enabled bool   `yaml:"enabled"`
	Level   string `yaml:"level"` // debug | info | warn | error | off
	Dir     string `yaml:"dir"`
	// MaxSizeMB rotates the current file when it grows past this size.
	MaxSizeMB int `yaml:"max_size_mb"`
	KeepFiles int `yaml:"keep_files"`
	// LLMDump writes the full (redacted) request payload of a failing model call
	// to <dir>/dumps — the fastest way to explain a vendor-specific rejection.
	LLMDump bool `yaml:"llm_dump"`
}

func (c *LoggingConfig) applyDefaults() {
	if c.Level == "" {
		c.Level = "info"
	}
	if c.Dir == "" {
		c.Dir = "logs"
	}
	if c.MaxSizeMB == 0 {
		c.MaxSizeMB = 32
	}
	if c.KeepFiles == 0 {
		c.KeepFiles = 5
	}
}

func (c LoggingConfig) Validate() error {
	switch strings.ToLower(strings.TrimSpace(c.Level)) {
	case "", "debug", "trace", "info", "warn", "warning", "error", "off", "none":
	default:
		return fmt.Errorf("logging.level must be debug, info, warn, error or off")
	}
	if c.MaxSizeMB < 0 || c.MaxSizeMB > 4096 {
		return fmt.Errorf("logging.max_size_mb must be between 0 and 4096")
	}
	if c.KeepFiles < 0 || c.KeepFiles > 100 {
		return fmt.Errorf("logging.keep_files must be between 0 and 100")
	}
	if strings.TrimSpace(c.Dir) == "" && c.Enabled {
		return fmt.Errorf("logging.dir is required when logging is enabled")
	}
	return nil
}
