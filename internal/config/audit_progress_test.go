package config

import (
	"strings"
	"testing"
)

func TestAuditProgressConfigDefaultsAndValidation(t *testing.T) {
	for _, tt := range []struct {
		name, body     string
		warning, limit int
		invalid        bool
	}{
		{name: "defaults", body: "workspace: .\n", warning: 8, limit: 16},
		{name: "custom", body: "agent:\n  audit_progress_warning_turns: 3\n  audit_progress_limit_turns: 7\n", warning: 3, limit: 7},
		{name: "warning zero", body: "agent:\n  audit_progress_warning_turns: 0\n", invalid: true},
		{name: "limit zero", body: "agent:\n  audit_progress_limit_turns: 0\n", invalid: true},
		{name: "negative", body: "agent:\n  audit_progress_warning_turns: -1\n", invalid: true},
		{name: "negative limit", body: "agent:\n  audit_progress_limit_turns: -1\n", invalid: true},
		{name: "equal", body: "agent:\n  audit_progress_warning_turns: 8\n  audit_progress_limit_turns: 8\n", invalid: true},
		{name: "inverted", body: "agent:\n  audit_progress_warning_turns: 9\n  audit_progress_limit_turns: 8\n", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadBudgetConfig(t, tt.body)
			if tt.invalid {
				if err == nil || !strings.Contains(err.Error(), "audit_progress") {
					t.Fatalf("expected progress validation error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w, l := cfg.Agent.AuditProgressTurns()
			if w != tt.warning || l != tt.limit {
				t.Fatalf("got %d/%d want %d/%d", w, l, tt.warning, tt.limit)
			}
		})
	}
	w, l := (AgentConfig{}).AuditProgressTurns()
	if w != 8 || l != 16 {
		t.Fatalf("zero config %d/%d", w, l)
	}
}
