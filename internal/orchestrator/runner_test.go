package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/provider"
	"code-review-agent/internal/schedule"
	"code-review-agent/internal/tools"
)

// fakeTeam records what the runner asked for and can simulate pause/complete.
type fakeTeam struct {
	mu           sync.Mutex
	workspaces   []string
	prompts      []string
	loads        []string
	saves        []string
	phase        string
	pauseReason  string
	pauseUntil   time.Time
	runBehavior  []string // one entry per Run call: "complete" | "pause" | "incomplete"
	workspaceErr error
	runCount     int
	afterRun     func(behavior string)
}

func (f *fakeTeam) SetWorkspace(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workspaceErr != nil {
		return f.workspaceErr
	}
	f.workspaces = append(f.workspaces, dir)
	return nil
}

func (f *fakeTeam) ConfigureBudget(bool, int, int, int64) error { return nil }

func (f *fakeTeam) Run(ctx context.Context, input string, emit func(agent.Event)) {
	f.mu.Lock()
	f.prompts = append(f.prompts, input)
	behavior := "complete"
	if f.runCount < len(f.runBehavior) {
		behavior = f.runBehavior[f.runCount]
	}
	f.runCount++
	f.pauseReason, f.pauseUntil = "", time.Time{}
	switch behavior {
	case "pause":
		f.phase = "audit"
		f.pauseReason = "所有启用源的 5 小时余量均已耗尽"
		f.pauseUntil = time.Now().Add(time.Hour)
	case "incomplete":
		f.phase = "audit"
	default:
		f.phase = "completed"
	}
	hook := f.afterRun
	f.mu.Unlock()
	if hook != nil {
		hook(behavior)
	}
	if emit != nil {
		emit(agent.Event{Kind: "info", Content: "fake run " + behavior})
	}
}

func (f *fakeTeam) SaveSession(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(`{"version":2}`), 0o600)
}

func (f *fakeTeam) LoadSession(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, path)
	return nil
}

func (f *fakeTeam) Snapshot() tools.Snapshot {
	return tools.Snapshot{Findings: []tools.Finding{{ID: 1, Severity: "high", Title: "X"}}}
}

func (f *fakeTeam) Revocations() []agent.FindingRevocation { return nil }

func (f *fakeTeam) PauseInfo() (string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseReason, f.pauseUntil
}

func (f *fakeTeam) ClearPause() {
	f.mu.Lock()
	f.pauseReason, f.pauseUntil = "", time.Time{}
	f.mu.Unlock()
}

func (f *fakeTeam) Phase() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.phase
}

type openGate struct{}

func (openGate) EarliestReset() provider.QuotaBudget { return provider.QuotaBudget{} }

func newTestRunner(t *testing.T, team Team) *Runner {
	t.Helper()
	dir := t.TempDir()
	cfg := config.OrchestratorConfig{TaskFile: filepath.Join(dir, "tasks.yaml"), StateFile: filepath.Join(dir, "state.json"),
		ReportDir: filepath.Join(dir, "reports"), SessionDir: filepath.Join(dir, "sessions"), ContinueOnFailure: true}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}, Quota: openGate{}}
	runner, err := NewRunner(team, cfg, gate, cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	runner.poll = time.Millisecond
	return runner
}

func makeDirs(t *testing.T, count int) []string {
	t.Helper()
	var dirs []string
	for i := 0; i < count; i++ {
		dir := filepath.Join(t.TempDir(), "repo"+string(rune('a'+i)))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

func TestRunnerProcessesQueueInOrder(t *testing.T) {
	dirs := makeDirs(t, 2)
	team := &fakeTeam{}
	runner := newTestRunner(t, team)
	for _, dir := range dirs {
		if _, err := runner.Import(dir, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(team.workspaces) != 2 || team.workspaces[0] != dirs[0] || team.workspaces[1] != dirs[1] {
		t.Fatalf("tasks ran out of order: %v", team.workspaces)
	}
	tasks := runner.Tasks()
	if len(tasks) != 2 || tasks[0].Status != StatusDone || tasks[1].Status != StatusDone {
		t.Fatalf("tasks not marked done: %+v", tasks)
	}
	for _, task := range tasks {
		if task.Report == "" {
			t.Fatalf("task %s has no report", task.ID)
		}
		data, err := os.ReadFile(task.Report)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"task"`) {
			t.Fatalf("report missing task context: %s", data)
		}
		md := filepath.Join(filepath.Dir(task.Report), "report.md")
		if _, err := os.Stat(md); err != nil {
			t.Fatalf("markdown report missing: %v", err)
		}
	}
	if summary := runner.Summary(); !strings.Contains(summary, "2/2 完成") {
		t.Fatalf("summary = %q", summary)
	}
}

// scriptedGate starts open and turns closed, mirroring a pool that exhausts its
// last source in the middle of a task.
type scriptedGate struct {
	mu   sync.Mutex
	open bool
}

func (g *scriptedGate) EarliestReset() provider.QuotaBudget {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return provider.QuotaBudget{}
	}
	return provider.QuotaBudget{EarliestReset: time.Now().Add(time.Hour), Reason: "所有启用源的 5 小时余量均已耗尽"}
}

func (g *scriptedGate) close() {
	g.mu.Lock()
	g.open = false
	g.mu.Unlock()
}

func TestRunnerPausesAndResumesTask(t *testing.T) {
	dirs := makeDirs(t, 1)
	gate := &scriptedGate{open: true}
	team := &fakeTeam{runBehavior: []string{"pause", "complete"}, afterRun: func(behavior string) {
		if behavior == "pause" {
			gate.close()
		}
	}}
	runner := newTestRunner(t, team)
	if _, err := runner.Import(dirs[0], false); err != nil {
		t.Fatal(err)
	}
	// The run exhausts the last source; the runner must park the task instead of
	// spinning on it until the 5h reset.
	runner.gate = &schedule.Gate{Schedule: &schedule.Schedule{}, Quota: gate}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = runner.Run(ctx, nil)
	tasks := runner.Tasks()
	if tasks[0].Status != StatusPaused {
		t.Fatalf("task should be paused after quota exhaustion, got %s", tasks[0].Status)
	}
	if len(team.saves) == 0 {
		t.Fatal("paused task must save its session for resume")
	}
	if tasks[0].Attempts != 0 {
		t.Fatalf("a quota pause must not consume a retry: %d", tasks[0].Attempts)
	}

	resumed := newTestRunnerForQueue(t, team, runner.statePath)
	resumed.gate = &schedule.Gate{Schedule: &schedule.Schedule{}, Quota: openGate{}}
	if err := resumed.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(team.loads) == 0 {
		t.Fatal("resumed task must load the saved session")
	}
	if !strings.Contains(team.prompts[len(team.prompts)-1], "继续基于当前团队状态") {
		t.Fatalf("resume prompt missing: %q", team.prompts[len(team.prompts)-1])
	}
	if got := resumed.Tasks()[0].Status; got != StatusDone {
		t.Fatalf("resumed task status = %s", got)
	}
}

func newTestRunnerForQueue(t *testing.T, team Team, statePath string) *Runner {
	t.Helper()
	cfg := config.OrchestratorConfig{TaskFile: statePath + ".yaml", StateFile: statePath,
		ReportDir: filepath.Join(filepath.Dir(statePath), "reports"), SessionDir: filepath.Join(filepath.Dir(statePath), "sessions"), ContinueOnFailure: true}
	gate := &schedule.Gate{Schedule: &schedule.Schedule{}, Quota: openGate{}}
	runner, err := NewRunner(team, cfg, gate, statePath)
	if err != nil {
		t.Fatal(err)
	}
	runner.poll = time.Millisecond
	return runner
}

func TestRunnerRetriesThenFails(t *testing.T) {
	dirs := makeDirs(t, 1)
	team := &fakeTeam{runBehavior: []string{"incomplete", "incomplete"}}
	runner := newTestRunner(t, team)
	runner.cfg.Retries = 1
	if _, err := runner.Import(dirs[0], false); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	tasks := runner.Tasks()
	if tasks[0].Status != StatusFailed {
		t.Fatalf("task should end failed after retries: %s (%s)", tasks[0].Status, tasks[0].Error)
	}
	if tasks[0].Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", tasks[0].Attempts)
	}
}

func TestRunnerStopsWhenGateClosed(t *testing.T) {
	dirs := makeDirs(t, 1)
	team := &fakeTeam{}
	runner := newTestRunner(t, team)
	if _, err := runner.Import(dirs[0], false); err != nil {
		t.Fatal(err)
	}
	closed := &schedule.Schedule{Enabled: true, Location: time.UTC,
		Windows: []schedule.Window{{StartMinute: 9 * 60, EndMinute: 10 * 60, Days: allWeekdays(), Label: "09:00-10:00"}}}
	runner.gate = &schedule.Gate{Schedule: closed, Quota: openGate{}, Now: func() time.Time {
		return time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = runner.Run(ctx, nil)
	if len(team.workspaces) != 0 {
		t.Fatal("no task may start while the working-hours window is closed")
	}
	if runner.Tasks()[0].Status != StatusPending {
		t.Fatalf("task must stay pending, got %s", runner.Tasks()[0].Status)
	}
}

func allWeekdays() map[time.Weekday]bool {
	days := map[time.Weekday]bool{}
	for day := time.Sunday; day <= time.Saturday; day++ {
		days[day] = true
	}
	return days
}

func TestImportFormatsAndDedupe(t *testing.T) {
	root := t.TempDir()
	repoA := filepath.Join(root, "a")
	repoB := filepath.Join(root, "b")
	for _, dir := range []string{repoA, repoB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	runner := newTestRunner(t, &fakeTeam{})
	listPath := filepath.Join(root, "tasks.txt")
	if err := os.WriteFile(listPath, []byte("# comment\n"+repoA+" | 先看鉴权\n"+repoB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := runner.Import(listPath, false)
	if err != nil || added != 2 {
		t.Fatalf("text import = %d, %v", added, err)
	}
	if runner.Tasks()[0].Prompt != "先看鉴权" {
		t.Fatalf("inline prompt lost: %+v", runner.Tasks()[0])
	}

	// Same directories again must not duplicate.
	if added, err := runner.Import(listPath, false); err != nil || added != 0 {
		t.Fatalf("dedupe failed: %d %v", added, err)
	}

	yamlPath := filepath.Join(root, "tasks.yaml")
	yaml := "tasks:\n  - dir: " + filepath.Join(root, "c") + "\n    title: C 项目\n"
	if err := os.MkdirAll(filepath.Join(root, "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if added, err := runner.Import(yamlPath, false); err != nil || added != 1 {
		t.Fatalf("yaml import = %d, %v", added, err)
	}
	if runner.Tasks()[2].Title != "C 项目" {
		t.Fatalf("yaml title lost: %+v", runner.Tasks()[2])
	}

	// Directory import takes every subdirectory as a task.
	dirRunner := newTestRunner(t, &fakeTeam{})
	if added, err := dirRunner.Import(root, true); err != nil || added != 3 {
		t.Fatalf("directory import = %d, %v", added, err)
	}
}

func TestImportRejectsMissingDirectory(t *testing.T) {
	runner := newTestRunner(t, &fakeTeam{})
	path := filepath.Join(t.TempDir(), "tasks.txt")
	if err := os.WriteFile(path, []byte("/definitely/not/here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Import(path, false); err == nil {
		t.Fatal("missing directories must be rejected")
	}
}

func TestQueuePersistenceAndRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	queue := newQueue()
	queue.Tasks = []Task{{ID: "T001", Dir: dir, Status: StatusRunning}, {Dir: dir + "/two"}}
	if err := queue.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tasks[0].Status != StatusPending {
		t.Fatalf("an interrupted running task must return to pending, got %s", loaded.Tasks[0].Status)
	}
	if loaded.Tasks[1].ID == "" {
		t.Fatal("missing ids must be assigned on load")
	}
	if loaded.Tasks[1].ID == loaded.Tasks[0].ID {
		t.Fatal("ids must stay unique")
	}
}

func TestSkipAndRetry(t *testing.T) {
	dirs := makeDirs(t, 1)
	runner := newTestRunner(t, &fakeTeam{})
	if _, err := runner.Import(dirs[0], false); err != nil {
		t.Fatal(err)
	}
	id := runner.Tasks()[0].ID
	if err := runner.Skip(id); err != nil {
		t.Fatal(err)
	}
	if runner.Tasks()[0].Status != StatusSkipped {
		t.Fatal("skip did not apply")
	}
	if err := runner.Retry(id); err != nil {
		t.Fatal(err)
	}
	if runner.Tasks()[0].Status != StatusPending {
		t.Fatal("retry did not requeue")
	}
	if err := runner.Skip("nope"); err == nil {
		t.Fatal("unknown id must fail")
	}
}

func TestWorkspaceFailureMarksTaskFailed(t *testing.T) {
	dirs := makeDirs(t, 1)
	team := &fakeTeam{workspaceErr: os.ErrPermission}
	runner := newTestRunner(t, team)
	if _, err := runner.Import(dirs[0], false); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	task := runner.Tasks()[0]
	if task.Status != StatusFailed || task.Error == "" {
		t.Fatalf("workspace failure not recorded: %+v", task)
	}
}
