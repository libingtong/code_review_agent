package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/orchestrator"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/provider"
	"code-review-agent/internal/schedule"
	"code-review-agent/internal/tools"
	tea "github.com/charmbracelet/bubbletea"
)

func newSourceModel(t *testing.T, providers []config.ProviderConfig) (*Model, *provider.Pool) {
	t.Helper()
	registry, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Providers: providers, Prompts: config.PromptConfig{System: "prompts/system.md"}}
	pool, err := provider.NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	team := agent.NewTeam(cfg, prompt.Prompts{}, pool, pool, registry)
	t.Cleanup(func() { _ = team.Close() })
	m := New(team, cfg, "")
	return &m, pool
}

func sourceProvider(id string, enabled bool) config.ProviderConfig {
	return config.ProviderConfig{ID: id, Label: strings.ToUpper(id), Enabled: &enabled, Weight: 1,
		OpenAI: config.OpenAIConfig{BaseURL: "https://example.invalid/v1", APIKey: "sk-x", Model: "m-" + id,
			MaxContextTokens: 1000, MaxOutputTokens: 100, APIInterface: "chat"},
		Quota: config.QuotaConfig{Window: "5h", Local: config.LocalQuotaConfig{Unit: "credits", Total: 100}}}
}

func TestProvidersModalShowsQuotaAndSwitches(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true), sourceProvider("mm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)

	cmd := m.openProviders()
	if cmd != nil {
		t.Fatal("opening the provider panel must not start work")
	}
	if m.modal == nil || m.modal.providers == nil {
		t.Fatal("provider panel did not open")
	}
	text := strings.Join(m.modal.lines, "\n")
	for _, needle := range []string{"GLM", "MM", "余量", "来源 local"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("provider panel missing %q:\n%s", needle, text)
		}
	}

	// Down + Enter forces a switch to the second source.
	m.updateProviders(tea.KeyMsg{Type: tea.KeyDown})
	m.updateProviders(tea.KeyMsg{Type: tea.KeyEnter})
	if pool.ActiveID() != "mm" {
		t.Fatalf("active provider = %q, want mm", pool.ActiveID())
	}

	// "e" disables the highlighted source but must refuse to disable the last one.
	m.updateProviders(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	statuses := pool.Statuses()
	if statuses[1].Enabled {
		t.Fatal("e did not disable the source")
	}
}

func TestProvidersPanelWithoutPool(t *testing.T) {
	registry, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	team := agent.NewTeam(cfg, prompt.Prompts{}, nil, nil, registry)
	defer team.Close()
	m := New(team, cfg, "")
	m.openProviders()
	if m.modal != nil {
		t.Fatal("no pool should not open a panel")
	}
	if len(m.events) == 0 || !strings.Contains(m.events[len(m.events)-1].content, "providers") {
		t.Fatalf("expected an explanatory event, got %+v", m.events)
	}
	if line := m.providerStatusLine(); line != "" {
		t.Fatalf("status line should be empty without a pool: %q", line)
	}
}

func TestTasksPanelAndCommands(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := orchestrator.NewRunner(nil, config.OrchestratorConfig{
		TaskFile: filepath.Join(dir, "tasks.yaml"), StateFile: filepath.Join(dir, "state.json"),
		ReportDir: filepath.Join(dir, "reports"), SessionDir: filepath.Join(dir, "sessions")},
		&schedule.Gate{Schedule: &schedule.Schedule{}, Quota: pool}, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, runner, nil)

	// Import through the command path so the parsing is exercised too.
	m.taskCommand("import " + repo)
	if len(runner.Tasks()) != 1 {
		t.Fatalf("import via command failed: %+v", runner.Tasks())
	}
	cmd := m.taskCommand("")
	if cmd != nil {
		t.Fatal("opening the task panel must not start work")
	}
	if m.modal == nil || m.modal.tasks == nil {
		t.Fatal("task panel did not open")
	}
	text := strings.Join(m.modal.lines, "\n")
	for _, needle := range []string{"待办", "repo", "import"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("task panel missing %q:\n%s", needle, text)
		}
	}

	m.updateTasks(tea.KeyMsg{Type: tea.KeyDown})
	m.updateTasks(tea.KeyMsg{Type: tea.KeyUp})
	if m.modal.tasks.selected != 0 {
		t.Fatalf("selection = %d", m.modal.tasks.selected)
	}

	if cmd := m.taskCommand("run"); cmd == nil {
		t.Fatal("run must return a command that drives the queue")
	}
	// The fake runner has a nil team, so the queue stays pending; stop it again.
	time.Sleep(10 * time.Millisecond)
	m.modal = nil
	runner.Stop()
	m.busy = false
}

func TestTasksCommandWithoutRunner(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)
	if cmd := m.taskCommand("run"); cmd != nil {
		t.Fatal("without an orchestrator nothing should start")
	}
	if len(m.events) == 0 || !strings.Contains(m.events[len(m.events)-1].content, "未启用") {
		t.Fatalf("expected a disabled notice, got %+v", m.events)
	}
}

func TestWindowEditorAddsAndPersists(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	store := filepath.Join(t.TempDir(), "schedule.json")
	m.cfg.Schedule = config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai", StoreFile: store}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}}
	m.AttachProviders(pool, gate, nil, nil)

	if cmd := m.openWindowPrompt(); cmd != nil {
		t.Fatal("opening the window editor must not start work")
	}
	if m.modal == nil || m.modal.window == nil {
		t.Fatal("window editor did not open")
	}
	// Add a window: prefilled 21:00-08:00 每天.
	m.updateWindow(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if !m.modal.window.editing {
		t.Fatal("a must enter the edit form")
	}
	m.updateWindow(tea.KeyMsg{Type: tea.KeyCtrlS})

	current := gate.CurrentSchedule()
	if current == nil || !current.Enabled || current.Describe() != "21:00-08:00" {
		t.Fatalf("window not applied: %+v", current)
	}
	if desc := current.Windows[0].DescribeDays(); desc != "每天" {
		t.Fatalf("weekday set = %q", desc)
	}
	if m.modal.window.editing {
		t.Fatal("Ctrl+S must return to the list")
	}

	// The edit must be persisted and reloadable across restarts.
	policy, ok, err := schedule.LoadPolicy(store)
	if err != nil || !ok {
		t.Fatalf("policy not saved: %v ok=%v", err, ok)
	}
	reloaded, err := schedule.LoadOrDefault(config.ScheduleConfig{StoreFile: store})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Describe() != "21:00-08:00" || len(policy.Windows) != 1 {
		t.Fatalf("persisted policy not reloaded: %s %+v", reloaded.Describe(), policy)
	}
}

func TestWindowEditorRejectsBadInput(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.cfg.Schedule = config.ScheduleConfig{Timezone: "Asia/Shanghai", StoreFile: filepath.Join(t.TempDir(), "s.json")}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}}
	m.AttachProviders(pool, gate, nil, nil)
	m.openWindowPrompt()
	m.updateWindow(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})

	m.modal.window.fields[0].SetValue("9pm")
	m.updateWindow(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.modal == nil || m.modal.err == "" {
		t.Fatal("invalid clock input must be reported instead of applied")
	}
	if gate.CurrentSchedule().Enabled {
		t.Fatal("invalid input must not change the live schedule")
	}

	m.modal.window.fields[0].SetValue("22:00")
	m.modal.window.fields[2].SetValue("funday")
	m.updateWindow(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.modal.err == "" {
		t.Fatal("invalid weekday must be reported")
	}
}

func TestWindowCommandSetsAndClears(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	store := filepath.Join(t.TempDir(), "schedule.json")
	m.cfg.Schedule = config.ScheduleConfig{Timezone: "Asia/Shanghai", StoreFile: store}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}}
	m.AttachProviders(pool, gate, nil, nil)

	m.windowCommand("22:00-06:00 mon-fri")
	current := gate.CurrentSchedule()
	if current == nil || !current.Enabled || current.Describe() != "22:00-06:00" {
		t.Fatalf("scripted window not applied: %+v", current)
	}
	if days := current.Windows[0].DescribeDays(); days != "mon-fri" {
		t.Fatalf("weekday restriction = %q", days)
	}
	if policy, ok, _ := schedule.LoadPolicy(store); !ok || len(policy.Windows) != 1 || policy.Windows[0].Start != "22:00" {
		t.Fatalf("scripted window not persisted: %+v", policy)
	}

	m.windowCommand("off")
	if gate.CurrentSchedule().Enabled {
		t.Fatal("off must disable the schedule")
	}
	// Crossing midnight still works after a scripted edit.
	m.windowCommand("21:00-08:00 all")
	if !gate.CurrentSchedule().Open(time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC)) {
		t.Fatal("cross-midnight window not honoured")
	}
}

func TestWindowDeleteKeepsGateValid(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.cfg.Schedule = config.ScheduleConfig{Timezone: "Asia/Shanghai", StoreFile: filepath.Join(t.TempDir(), "s.json")}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}}
	m.AttachProviders(pool, gate, nil, nil)
	m.openWindowPrompt()
	m.updateWindow(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m.updateWindow(tea.KeyMsg{Type: tea.KeyCtrlS})

	m.updateWindow(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	current := gate.CurrentSchedule()
	if current == nil || len(current.Windows) != 0 {
		t.Fatalf("window not deleted: %+v", current)
	}
	if current.Enabled {
		t.Fatal("deleting the last window must disable the policy instead of blocking forever")
	}
	if m.modal.err != "" {
		t.Fatalf("deleting the last window must not error: %s", m.modal.err)
	}
}

func TestQuotaViewWithoutPool(t *testing.T) {
	registry, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	team := agent.NewTeam(cfg, prompt.Prompts{}, nil, nil, registry)
	defer team.Close()
	m := New(team, cfg, "")
	m.showQuota("")
	if len(m.events) == 0 || !strings.Contains(m.events[len(m.events)-1].content, "多源") {
		t.Fatalf("expected a multi-source notice, got %+v", m.events)
	}
	if m.modal != nil {
		t.Fatal("no pool means no modal")
	}
}

func TestManualRunHonoursGateButGoOverrides(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	closed := &schedule.Schedule{Enabled: true, Location: time.UTC,
		Windows: []schedule.Window{{StartMinute: 21 * 60, EndMinute: 23 * 60, Days: daySet(), Label: "21:00-23:00"}}}
	m.AttachProviders(pool, &schedule.Gate{Schedule: closed, Quota: nil, Now: func() time.Time {
		return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	}}, nil, nil)

	if cmd := m.runQuery("请审计"); cmd != nil {
		t.Fatal("a manual run outside the window must be refused")
	}
	if len(m.events) == 0 || !strings.Contains(m.events[len(m.events)-1].content, "当前暂停") {
		t.Fatalf("expected a pause explanation, got %+v", m.events)
	}

	// go is the explicit override and must still start.
	if cmd := m.resumeRun("继续"); cmd == nil {
		t.Fatal("go must override the gate")
	}
	m.cancel()
	m.busy = false
}

func daySet() map[time.Weekday]bool {
	days := map[time.Weekday]bool{}
	for day := time.Sunday; day <= time.Saturday; day++ {
		days[day] = true
	}
	return days
}

func TestSourceNotesAppearAsEvents(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true), sourceProvider("mm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)
	if err := pool.SetActive("mm"); err != nil {
		t.Fatal(err)
	}
	m.syncTeam()
	found := false
	for _, event := range m.events {
		if strings.Contains(event.content, "源切换") && strings.Contains(event.content, "glm → mm") {
			found = true
		}
	}
	if !found {
		t.Fatalf("switch note missing from the event stream: %+v", m.events)
	}
}

func TestProviderPanelShowsConcurrency(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)
	m.openProviders()
	text := strings.Join(m.modal.lines, "\n")
	if !strings.Contains(text, "并发") {
		t.Fatalf("provider panel must show adaptive concurrency:\n%s", text)
	}
}

func TestLimitCommandSetsAndClearsCap(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	pool.SetLimitsPath(filepath.Join(t.TempDir(), "limits.json"))
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)

	m.limitCommand("glm 90%")
	if ratio, _, ok := pool.UsageLimit("glm"); !ok || ratio != 0.9 {
		t.Fatalf("limit not applied: %v ok=%v", ratio, ok)
	}
	if m.modal != nil {
		t.Fatal("listing is only for the no-argument form")
	}

	// The panel shows the cap and the remaining usable amount.
	m.openProviders()
	text := strings.Join(m.modal.lines, "\n")
	if !strings.Contains(text, "自设上限 90%") {
		t.Fatalf("panel missing the cap:\n%s", text)
	}

	m.modal = nil
	m.limitCommand("glm off")
	if ratio, _, _ := pool.UsageLimit("glm"); ratio != 1 {
		t.Fatalf("off must restore the full window, got %v", ratio)
	}

	m.limitCommand("glm reserve 500")
	if _, reserve, _ := pool.UsageLimit("glm"); reserve != 500 {
		t.Fatalf("reserve not applied: %v", reserve)
	}

	m.limitCommand("glm 120%")
	if ratio, _, _ := pool.UsageLimit("glm"); ratio != 1 {
		t.Fatalf("invalid ratio must be rejected, got %v", ratio)
	}
	if len(m.events) == 0 || !strings.Contains(m.events[len(m.events)-1].content, "用量上限写成") {
		t.Fatalf("invalid input should be explained: %+v", m.events)
	}
}

func TestLimitListing(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Schedule: &schedule.Schedule{}}, nil, nil)
	m.limitCommand("")
	if m.modal == nil {
		t.Fatal("no-argument /limit must open the listing")
	}
	text := strings.Join(m.modal.lines, "\n")
	if !strings.Contains(text, "未设上限") {
		t.Fatalf("listing should state the default:\n%s", text)
	}
}

func TestWindowCommandOverridesOneChannel(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true), sourceProvider("mm", true)})
	m.cfg.Schedule = config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		StoreFile: filepath.Join(t.TempDir(), "schedule.json")}
	plan := schedule.NewPlan(nil)
	m.AttachProviders(pool, &schedule.Gate{Quota: pool}, nil, nil)
	m.gate.SetPlan(plan)

	// Global window: nights only.
	m.windowCommand("21:00-08:00 all")
	if !m.gate.Plan().Overridden("") == false {
		t.Fatal("global edit must not create a channel override")
	}

	// GLM around the clock; mm keeps following the global window.
	m.windowCommand("glm always")
	if !m.gate.Plan().Overridden("glm") {
		t.Fatal("override not installed")
	}
	day := time.Date(2026, 11, 7, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !m.gate.Plan().OpenAt("glm", day) {
		t.Fatal("glm must be usable during the day")
	}
	if m.gate.Plan().OpenAt("mm", day) {
		t.Fatal("mm must follow the global night window")
	}
	if decision := m.gate.Check(); !decision.Allowed {
		t.Fatalf("gate must allow work while any channel is open: %+v", decision)
	}

	// restore
	m.windowCommand("glm inherit")
	if m.gate.Plan().Overridden("glm") {
		t.Fatal("override not removed")
	}
	if m.gate.Plan().OpenAt("glm", day) {
		t.Fatal("glm must follow the global window again")
	}
}

func TestChannelWindowCommandRejectsUnknownChannel(t *testing.T) {
	m, pool := newSourceModel(t, []config.ProviderConfig{sourceProvider("glm", true)})
	m.AttachProviders(pool, &schedule.Gate{Quota: pool}, nil, nil)
	// "nope" is not a channel, so it is parsed as a global clock and rejected.
	m.windowCommand("nope always")
	if len(m.events) == 0 {
		t.Fatal("invalid command must be reported")
	}
	last := m.events[len(m.events)-1].content
	if !strings.Contains(last, "用法") && !strings.Contains(last, "失败") {
		t.Fatalf("unexpected response: %q", last)
	}
}
