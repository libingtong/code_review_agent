package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/report"
	"code-review-agent/internal/schedule"
	"code-review-agent/internal/tools"
)

// Team is the audit engine the runner drives. It is an interface so the queue
// logic can be tested without a live model.
type Team interface {
	SetWorkspace(string) error
	ConfigureBudget(infinite bool, hours, minutes int, tokens int64) error
	Run(ctx context.Context, input string, emit func(agent.Event))
	SaveSession(string) error
	LoadSession(string) error
	Snapshot() tools.Snapshot
	Revocations() []agent.FindingRevocation
	PauseInfo() (string, time.Time)
	ClearPause()
	Phase() string
}

const defaultPrompt = "请对当前工作区进行安全审计。先独立侦察并通过论坛交接，再由审计团队验证和报告有证据的问题。工作区："

// Runner processes the queue serially. A Team run is single-flight, so
// concurrency stays at 1 by design rather than by omission.
type Runner struct {
	mu        sync.Mutex
	team      Team
	gate      *schedule.Gate
	cfg       config.OrchestratorConfig
	queue     *Queue
	statePath string
	poll      time.Duration
	now       func() time.Time
	emit      func(agent.Event)
	running   bool
	cancel    context.CancelFunc
}

func NewRunner(team Team, cfg config.OrchestratorConfig, gate *schedule.Gate, statePath string) (*Runner, error) {
	if cfg.TaskFile == "" {
		cfg.TaskFile = "tasks.yaml"
	}
	if cfg.StateFile == "" {
		cfg.StateFile = "orchestrator/state.json"
	}
	if cfg.ReportDir == "" {
		cfg.ReportDir = "reports"
	}
	if cfg.SessionDir == "" {
		cfg.SessionDir = filepath.Join("sessions", "tasks")
	}
	if statePath == "" {
		statePath = cfg.StateFile
	}
	queue, err := LoadQueue(statePath)
	if err != nil {
		return nil, err
	}
	return &Runner{team: team, gate: gate, cfg: cfg, queue: queue, statePath: statePath,
		poll: 30 * time.Second, now: time.Now}, nil
}

func (r *Runner) SetEmit(emit func(agent.Event)) {
	r.mu.Lock()
	r.emit = emit
	r.mu.Unlock()
}

// Tasks returns a copy so callers cannot mutate scheduling state.
func (r *Runner) Tasks() []Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Task(nil), r.queue.Tasks...)
}

// Summary is the one-line progress indicator for the TUI header.
func (r *Runner) Summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue.Tasks) == 0 {
		return "无任务"
	}
	counts := r.queue.counts()
	return fmt.Sprintf("%d/%d 完成 · %d 待办 · %d 失败",
		counts[StatusDone], len(r.queue.Tasks), counts[StatusPending]+counts[StatusPaused], counts[StatusFailed])
}

func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Import merges tasks from a file or directory into the queue. eachSubdir
// expands a directory into one task per repository.
func (r *Runner) Import(path string, eachSubdir bool) (int, error) {
	tasks, err := importPath(path, eachSubdir)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing := map[string]bool{}
	for _, task := range r.queue.Tasks {
		existing[task.Dir] = true
	}
	added := 0
	for _, task := range tasks {
		if task.Dir == "" {
			continue
		}
		if info, statErr := os.Stat(task.Dir); statErr != nil || !info.IsDir() {
			return added, fmt.Errorf("任务目录不存在：%s", task.Dir)
		}
		if existing[task.Dir] {
			continue
		}
		existing[task.Dir] = true
		task.ID = fmt.Sprintf("T%03d", len(r.queue.Tasks)+1)
		task.Status = StatusPending
		if task.Title == "" {
			task.Title = filepath.Base(task.Dir)
		}
		if abs, err := filepath.Abs(task.Dir); err == nil {
			task.Dir = abs
		}
		r.queue.Tasks = append(r.queue.Tasks, task)
		added++
	}
	r.queue.normalize()
	if err := r.queue.Save(r.statePath); err != nil {
		return added, err
	}
	return added, nil
}

// Skip marks one task as skipped so the queue can move on.
func (r *Runner) Skip(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	task := r.queue.byID(strings.TrimSpace(id))
	if task == nil {
		return fmt.Errorf("任务不存在：%s", id)
	}
	if task.Status == StatusRunning {
		return fmt.Errorf("任务 %s 正在运行，请先停止", id)
	}
	task.Status = StatusSkipped
	task.EndedAt = r.now().Format(time.RFC3339)
	return r.queue.Save(r.statePath)
}

// Retry puts a finished task back into the queue.
func (r *Runner) Retry(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	task := r.queue.byID(strings.TrimSpace(id))
	if task == nil {
		return fmt.Errorf("任务不存在：%s", id)
	}
	task.Status = StatusPending
	task.Attempts = 0
	task.Error = ""
	task.EndedAt = ""
	return r.queue.Save(r.statePath)
}

// Stop cancels an in-flight run. Progress is kept in the queue state.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Runner) publish(e agent.Event) {
	r.mu.Lock()
	emit := r.emit
	r.mu.Unlock()
	if emit != nil {
		emit(e)
	}
}

func (r *Runner) info(format string, args ...any) {
	r.publish(agent.Event{Kind: "info", Content: fmt.Sprintf(format, args...)})
}

// Run processes tasks until the queue is empty, the gate closes, or ctx ends.
// It returns nil on a clean drain so the caller can distinguish "finished" from
// "interrupted".
func (r *Runner) Run(ctx context.Context, emit func(agent.Event)) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("任务队列已在运行")
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.running = true
	r.emit = emit
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.mu.Unlock()
		cancel()
	}()

	if err := r.waitForGate(runCtx); err != nil {
		return err
	}
	for {
		task := r.nextTask()
		if task == nil {
			r.info("任务队列已全部完成")
			return nil
		}
		outcome := r.runTask(runCtx, task)
		switch outcome {
		case outcomeDone, outcomeFailed:
			continue
		case outcomeStopped:
			return nil
		case outcomePaused:
			if err := r.waitForGate(runCtx); err != nil {
				return err
			}
		}
	}
}

func (r *Runner) nextTask() *Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	task := r.queue.next()
	if task == nil {
		return nil
	}
	snapshot := *task
	return &snapshot
}

// waitForGate blocks until both the working-hours window and provider quota
// allow work. It never spins: the wait is bounded by poll or the resume time.
func (r *Runner) waitForGate(ctx context.Context) error {
	announced := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		decision := r.gate.Check()
		if decision.Allowed {
			if announced {
				r.info("继续执行任务队列")
			}
			return nil
		}
		if !announced {
			announced = true
			if decision.ResumeAt.IsZero() {
				r.info("暂停：%s", decision.Reason)
			} else {
				r.info("暂停：%s，预计 %s 继续", decision.Reason, decision.ResumeAt.Local().Format("01-02 15:04"))
			}
		}
		wait := r.poll
		if !decision.ResumeAt.IsZero() {
			if until := time.Until(decision.ResumeAt); until > 0 && until < wait {
				wait = until
			}
		}
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type outcome int

const (
	outcomeDone outcome = iota
	outcomeFailed
	outcomePaused
	outcomeStopped
)

// update mutates the queue's own task entry. Mutating a snapshot copy would
// leave the stored task pending forever and re-run it in an endless loop.
func (r *Runner) update(id string, mutate func(*Task)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if task := r.queue.byID(id); task != nil {
		mutate(task)
	}
}

func (r *Runner) runTask(ctx context.Context, task *Task) outcome {
	if err := r.team.SetWorkspace(task.Dir); err != nil {
		return r.fail(task, err)
	}
	if err := r.team.ConfigureBudget(r.budgetSetting(task)); err != nil {
		return r.fail(task, err)
	}
	sessionPath := filepath.Join(r.cfg.SessionDir, task.ID+".json")
	resume := false
	if info, err := os.Stat(sessionPath); err == nil && !info.IsDir() {
		resume = true
	}
	if resume {
		if err := r.team.LoadSession(sessionPath); err != nil {
			r.info("任务 %s 无法恢复会话（%v），改为全新审计", task.ID, err)
			resume = false
		}
	}
	prompt := strings.TrimSpace(task.Prompt)
	if resume {
		prompt = "继续基于当前团队状态和论坛审计，不要从头开始。" + prompt
	} else if prompt == "" {
		prompt = defaultPrompt + task.Dir
	}

	attempts := 0
	r.update(task.ID, func(stored *Task) {
		stored.Status = StatusRunning
		stored.Attempts++
		stored.StartedAt = r.now().Format(time.RFC3339)
		stored.Session = sessionPath
		stored.Error = ""
		attempts = stored.Attempts
	})
	_ = r.queue.Save(r.statePath)
	r.team.ClearPause()

	r.team.Run(ctx, prompt, r.publish)
	phase := strings.TrimSpace(r.team.Phase())
	reason, until := r.team.PauseInfo()

	if phase == "completed" {
		if err := r.team.SaveSession(sessionPath); err != nil {
			r.info("任务 %s 保存会话失败：%v", task.ID, err)
		}
		reportPath, exportErr := r.exportReport(task)
		r.update(task.ID, func(stored *Task) {
			stored.Status = StatusDone
			stored.EndedAt = r.now().Format(time.RFC3339)
			stored.Report = reportPath
			if exportErr != nil {
				stored.Error = "报告导出失败：" + exportErr.Error()
			}
		})
		_ = r.queue.Save(r.statePath)
		r.info("任务 %s 完成", task.ID)
		return outcomeDone
	}

	if reason != "" || !until.IsZero() {
		// Quota pause: keep the session so the next attempt resumes in place and
		// do not consume a retry.
		if err := r.team.SaveSession(sessionPath); err != nil {
			r.info("任务 %s 暂停时保存会话失败：%v", task.ID, err)
		}
		r.update(task.ID, func(stored *Task) {
			stored.Status = StatusPaused
			stored.Error = reason
			if stored.Attempts > 0 {
				stored.Attempts--
			}
		})
		_ = r.queue.Save(r.statePath)
		r.info("任务 %s 因余量暂停，进度已保存：%s", task.ID, reason)
		return outcomePaused
	}

	if ctx.Err() != nil {
		r.update(task.ID, func(stored *Task) { stored.Status = StatusPending })
		_ = r.queue.Save(r.statePath)
		r.info("任务 %s 已停止，进度保留", task.ID)
		return outcomeStopped
	}

	// Task-level 0 means "inherit the queue default"; -1 disables retries.
	retries := task.Retries
	if retries == 0 {
		retries = r.cfg.Retries
	}
	if retries < 0 {
		retries = 0
	}
	if attempts > retries {
		var failure string
		r.update(task.ID, func(stored *Task) {
			stored.Status = StatusFailed
			stored.EndedAt = r.now().Format(time.RFC3339)
			if stored.Error == "" {
				stored.Error = "团队未完成且未因余量暂停"
			}
			failure = stored.Error
		})
		_ = r.queue.Save(r.statePath)
		r.info("任务 %s 失败：%s", task.ID, failure)
		if !r.cfg.ContinueOnFailure {
			return outcomeStopped
		}
		return outcomeFailed
	}
	if err := r.team.SaveSession(sessionPath); err == nil {
		r.update(task.ID, func(stored *Task) { stored.Status = StatusPending })
	}
	_ = r.queue.Save(r.statePath)
	r.info("任务 %s 未完成，第 %d 次尝试结束，将重试", task.ID, attempts)
	return outcomeFailed
}

func (r *Runner) fail(task *Task, err error) outcome {
	r.update(task.ID, func(stored *Task) {
		stored.Status = StatusFailed
		stored.Error = err.Error()
		stored.EndedAt = r.now().Format(time.RFC3339)
	})
	_ = r.queue.Save(r.statePath)
	r.info("任务 %s 无法启动：%v", task.ID, err)
	if !r.cfg.ContinueOnFailure {
		return outcomeStopped
	}
	return outcomeFailed
}

func (r *Runner) budgetSetting(task *Task) (bool, int, int, int64) {
	infinite := true
	if task.Budget.Infinite != nil {
		infinite = *task.Budget.Infinite
	}
	return infinite, task.Budget.Hours, task.Budget.Minutes, task.Budget.Tokens
}

func (r *Runner) exportReport(task *Task) (string, error) {
	dir := filepath.Join(r.cfg.ReportDir, task.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	snapshot := r.team.Snapshot()
	built := report.Build(snapshot, r.team.Revocations())
	built.Task = &report.TaskInfo{ID: task.ID, Title: task.Title, Dir: task.Dir}
	jsonPath := filepath.Join(dir, "report.json")
	mdPath := filepath.Join(dir, "report.md")
	if err := report.WriteJSON(jsonPath, built); err != nil {
		return "", err
	}
	if err := report.WriteMarkdown(mdPath, built); err != nil {
		return "", err
	}
	return jsonPath, nil
}
