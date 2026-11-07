package tui

import (
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/schedule"
	tea "github.com/charmbracelet/bubbletea"
)

func TestCommandTokenOnlyCompletesTheCommand(t *testing.T) {
	cases := []struct {
		value string
		token string
		ok    bool
	}{
		{"/", "", true},
		{"/wi", "wi", true},
		{"/window", "window", true},
		{"  /mode", "mode", true},
		{"/window 21:00-08:00", "", false},
		{"go", "", false},
		{"", "", false},
	}
	for _, test := range cases {
		token, ok := commandToken(test.value)
		if ok != test.ok || token != test.token {
			t.Fatalf("%q: got (%q,%v) want (%q,%v)", test.value, token, ok, test.token, test.ok)
		}
	}
}

func TestMatchesCommandsFiltersAndSorts(t *testing.T) {
	all := matchesCommands("")
	if len(all) == 0 || len(all) > maxSuggestions {
		t.Fatalf("empty token should list up to %d commands, got %d", maxSuggestions, len(all))
	}
	window := matchesCommands("win")
	if len(window) != 1 || window[0].Name != "window" {
		t.Fatalf("win → %+v", window)
	}
	// Every spec has a description, so the menu is useful without /help.
	for _, spec := range matchesCommands("") {
		if spec.Desc == "" {
			t.Fatalf("command %q has no description", spec.Name)
		}
	}
}

func TestTabCompletesAndEscDismisses(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)

	m.input.SetValue("/win")
	m.refreshSuggestions()
	if len(m.suggestions) != 1 || m.suggestions[0].Name != "window" {
		t.Fatalf("suggestions = %+v", m.suggestions)
	}
	if _, handled := m.handleKey(tea.KeyMsg{Type: tea.KeyTab}); !handled {
		t.Fatal("Tab must be consumed by the menu")
	}
	if got := m.input.Value(); got != "/window " {
		t.Fatalf("completion inserted %q", got)
	}

	// A slash alone opens the menu; Esc closes it without pausing the run.
	m.input.SetValue("/")
	m.refreshSuggestions()
	if len(m.suggestions) == 0 {
		t.Fatal("menu should open on /")
	}
	if _, handled := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled {
		t.Fatal("Esc should be consumed while the menu is open")
	}
	if len(m.suggestions) != 0 {
		t.Fatalf("menu not dismissed: %+v", m.suggestions)
	}
	if m.escapeArmed {
		t.Fatal("dismissing the menu must not arm the pause shortcut")
	}
}

func TestCompletionMenuRendersDescriptions(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)
	m.input.SetValue("/mo")
	m.refreshSuggestions()
	lines := m.suggestionLines(80)
	if len(lines) != 1 || !strings.Contains(lines[0], "/mode") || !strings.Contains(lines[0], "渠道") {
		t.Fatalf("menu line = %v", lines)
	}
	if !strings.Contains(lines[0], ">") {
		t.Fatalf("selected row should be marked: %q", lines[0])
	}
}

// Esc must always close a panel: mode/providers/tasks own their keys, and a
// missing escape hatch is a dead end for the user.
func TestEscClosesEveryPanel(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)

	open := map[string]func(){
		"mode":        func() { m.modeCommand("") },
		"providers":   func() { m.openProviders() },
		"tasks":       func() { m.modal = &modalState{title: "t", tasks: &tasksModal{}} },
		"window":      func() { m.openWindowPrompt() },
		"concurrency": func() { m.concurrencyCommand("") },
		"quota":       func() { m.showQuota("") },
	}
	for name, opener := range open {
		m.modal = nil
		opener()
		if m.modal == nil {
			t.Fatalf("%s: panel did not open", name)
		}
		m.updateModal(tea.KeyMsg{Type: tea.KeyEsc})
		if m.modal != nil {
			t.Fatalf("%s: Esc did not close the panel", name)
		}
	}
}
