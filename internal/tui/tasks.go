package tui

import (
	"context"
	"fmt"
	"strings"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/orchestrator"
	tea "github.com/charmbracelet/bubbletea"
)

// tasksModal shows the queue. Mutating operations are commands so they stay
// scriptable and testable; the panel itself is read-only.
type tasksModal struct {
	selected int
}

func (m *Model) openTasks() tea.Cmd {
	if m.orchestrator == nil {
		m.addEvent("任务编排未启用：config.yaml 的 orchestrator 段不可用。")
		return nil
	}
	m.modal = &modalState{title: "任务队列 · /tasks", tasks: &tasksModal{}}
	m.modal.lines = m.taskLines()
	m.modal.wrapWidth = 0
	m.input.Blur()
	m.resizeModal()
	return nil
}

func (m *Model) taskLines() []string {
	if m.orchestrator == nil {
		return []string{"任务编排未启用"}
	}
	tasks := m.orchestrator.Tasks()
	lines := []string{
		m.orchestrator.Summary(),
		"",
		"/tasks import <文件或目录> [--each-subdir] · /tasks run · /tasks stop · /tasks skip <ID> · /tasks retry <ID>",
		"",
	}
	if len(tasks) == 0 {
		return append(lines, "队列为空。导入后运行即可无人值守执行。")
	}
	for index, task := range tasks {
		marker := "  "
		if index == m.modal.tasks.selected {
			marker = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%s [%s] %s", marker, task.ID, statusText(string(task.Status)), boundedText(task.Title, 80)))
		lines = append(lines, "    "+boundedText(task.Dir, 200))
		detail := fmt.Sprintf("    尝试 %d · 重试上限 %d", task.Attempts, task.Retries)
		if task.Report != "" {
			detail += " · 报告 " + task.Report
		}
		lines = append(lines, detail)
		if task.Error != "" {
			lines = append(lines, "    "+boundedText(task.Error, 300))
		}
		lines = append(lines, "")
	}
	return lines
}

func statusText(status string) string {
	switch status {
	case string(orchestrator.StatusDone):
		return "完成"
	case string(orchestrator.StatusRunning):
		return "运行中"
	case string(orchestrator.StatusPaused):
		return "已暂停"
	case string(orchestrator.StatusFailed):
		return "失败"
	case string(orchestrator.StatusSkipped):
		return "已跳过"
	}
	return "待办"
}

func (m *Model) refreshTasksModal() {
	if m.modal == nil || m.modal.tasks == nil {
		return
	}
	m.modal.lines = m.taskLines()
	m.modal.wrapWidth = 0
	m.resizeModal()
}

func (m *Model) updateTasks(msg tea.Msg) tea.Cmd {
	state := m.modal.tasks
	tasks := m.orchestrator.Tasks()
	if len(tasks) == 0 {
		return nil
	}
	switch key := msg.(type) {
	case tea.KeyMsg:
		switch key.String() {
		case "up", "k":
			state.selected--
		case "down", "j":
			state.selected++
		}
	case tea.MouseMsg:
		if key.Type == tea.MouseWheelUp {
			state.selected--
		}
		if key.Type == tea.MouseWheelDown {
			state.selected++
		}
	}
	state.selected = min(max(0, state.selected), len(tasks)-1)
	m.refreshTasksModal()
	return nil
}

// taskCommand implements /tasks <verb>. It returns a tea.Cmd only for `run`,
// which owns the mailbox for the lifetime of the queue.
func (m *Model) taskCommand(arg string) tea.Cmd {
	if m.orchestrator == nil {
		m.addEvent("任务编排未启用。")
		return nil
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return m.openTasks()
	}
	verb := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(arg, fields[0]))
	switch verb {
	case "list":
		return m.openTasks()
	case "import":
		if rest == "" {
			m.addEvent("用法：/tasks import <文件或目录> [--each-subdir]")
			return nil
		}
		each := false
		if strings.Contains(rest, "--each-subdir") {
			each = true
			rest = strings.TrimSpace(strings.ReplaceAll(rest, "--each-subdir", ""))
		}
		path := cleanInputDir(rest)
		added, err := m.orchestrator.Import(path, each)
		if err != nil {
			m.addEvent("导入失败：" + err.Error())
			return nil
		}
		m.addEvent(fmt.Sprintf("已导入 %d 个任务（当前 %d 个）", added, len(m.orchestrator.Tasks())))
		m.refreshTasksModal()
	case "run":
		return m.runTasks()
	case "stop":
		m.orchestrator.Stop()
		m.addEvent("正在停止任务队列；当前任务进度会保留。")
	case "skip":
		if err := m.orchestrator.Skip(strings.TrimSpace(rest)); err != nil {
			m.addEvent("跳过失败：" + err.Error())
			return nil
		}
		m.addEvent("已跳过任务 " + strings.TrimSpace(rest))
		m.refreshTasksModal()
	case "retry":
		if err := m.orchestrator.Retry(strings.TrimSpace(rest)); err != nil {
			m.addEvent("重试失败：" + err.Error())
			return nil
		}
		m.addEvent("已重新排队任务 " + strings.TrimSpace(rest))
		m.refreshTasksModal()
	default:
		m.addEvent("未知的 /tasks 子命令；可用 import、run、stop、skip、retry、list。")
	}
	return nil
}

// runTasks drives the whole queue through the same mailbox the interactive
// audit uses, so progress, forum posts and pause reasons all render normally.
func (m *Model) runTasks() tea.Cmd {
	if m.busy || m.saving {
		m.addEvent("请等待当前运行或保存完成后再启动任务队列。")
		return nil
	}
	if len(m.orchestrator.Tasks()) == 0 {
		m.addEvent("任务队列为空；先 /tasks import <目录或文件>。")
		return nil
	}
	m.busy = true
	m.stopping = false
	m.session++
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	box := &eventMailbox{wake: make(chan struct{}, 1)}
	m.mailbox = box
	tasks := m.orchestrator
	go func() {
		defer func() {
			failure := ""
			if r := recover(); r != nil {
				failure = fmt.Sprint(r)
			}
			box.finish(failure)
		}()
		_ = tasks.Run(ctx, func(e agent.Event) {
			box.post(e)
		})
	}()
	m.addEvent("任务队列已启动；Esc 暂停，/tasks 查看进度。")
	return waitAgent(m.session, box)
}
