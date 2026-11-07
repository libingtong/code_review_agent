package tui

import (
	"sort"
	"strings"
)

// commandSpec is the single source of truth for command completion. Keep it in
// sync with the switch in commands.go — /help and Tab completion both read it.
type commandSpec struct {
	Name string
	Args string
	Desc string
}

var commandSpecs = []commandSpec{
	{"agents", "", "Agent 实时列表与诊断"},
	{"budget", "", "审计预算（时间 / token）"},
	{"concurrency", "[reset [ID]]", "查看或清除并发学习值"},
	{"dir", "<目录>", "切换审计目录"},
	{"export", "[文件]", "导出完整 JSON 报告"},
	{"files", "[页码]", "文件覆盖详情"},
	{"forum", "[页码]", "论坛帖子列表"},
	{"go", "", "暂停后继续当前团队"},
	{"help", "", "操作帮助"},
	{"limit", "[ID] [90%|off|reserve N]", "自设用量上限"},
	{"list", "[页码]", "漏洞列表"},
	{"mode", "[渠道] [模型]", "选择渠道/模型（持久化）"},
	{"providers", "[ID]", "渠道列表与切换"},
	{"quota", "[ID]", "5 小时余量与探针原始响应"},
	{"reply", "<ID> <内容>", "回复论坛帖子"},
	{"report", "", "审计报告摘要"},
	{"restore", "[文件/片段]", "恢复已保存会话"},
	{"save", "[文件]", "保存当前会话"},
	{"say", "[消息]", "向所有 Agent 广播"},
	{"search", "<关键词>", "搜索论坛"},
	{"session", "", "选择并恢复会话"},
	{"sessions", "", "选择并恢复会话"},
	{"status", "", "回到运行事件面板"},
	{"tasks", "[import|run|stop|skip|retry]", "任务队列"},
	{"thread", "<ID>", "展开帖子与回复"},
	{"window", "[off|on|HH:MM-HH:MM|渠道 ...]", "工作时间段"},
}

const maxSuggestions = 6

// matchesCommands returns commands whose name starts with the typed token.
func matchesCommands(token string) []commandSpec {
	token = strings.ToLower(strings.TrimSpace(token))
	var out []commandSpec
	for _, spec := range commandSpecs {
		if token == "" || strings.HasPrefix(spec.Name, token) {
			out = append(out, spec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Exact prefix matches first, then alphabetical.
		if out[i].Name == token || out[j].Name == token {
			return out[i].Name == token
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}

// commandToken returns the text being completed: only the leading "/word" part
// counts, so arguments never trigger command completion.
func commandToken(value string) (string, bool) {
	value = strings.TrimLeft(value, " ")
	if !strings.HasPrefix(value, "/") {
		return "", false
	}
	trimmed := value[1:]
	if trimmed == "" {
		return "", true
	}
	if index := strings.IndexAny(trimmed, " \t"); index >= 0 {
		return "", false
	}
	return strings.ToLower(trimmed), true
}

// refreshSuggestions recomputes the completion list from the current input.
func (m *Model) refreshSuggestions() {
	m.suggestions = nil
	m.suggestIndex = 0
	if m.modal != nil {
		return
	}
	token, ok := commandToken(m.input.Value())
	if !ok {
		return
	}
	m.suggestions = matchesCommands(token)
	// A single exact match needs no menu.
	if len(m.suggestions) == 1 && m.suggestions[0].Name == token && m.input.Value() == "/"+token {
		m.suggestions = nil
	}
}

// acceptSuggestion inserts the highlighted command (plus a space for commands
// that take arguments) and keeps the list for further Tab presses.
func (m *Model) acceptSuggestion() bool {
	if len(m.suggestions) == 0 {
		return false
	}
	spec := m.suggestions[min(max(0, m.suggestIndex), len(m.suggestions)-1)]
	value := "/" + spec.Name
	if spec.Args != "" {
		value += " "
	}
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.refreshSuggestions()
	return true
}

// suggestionLines renders the completion menu; empty when nothing to show.
func (m Model) suggestionLines(width int) []string {
	if len(m.suggestions) == 0 {
		return nil
	}
	lines := make([]string, 0, len(m.suggestions)+1)
	for index, spec := range m.suggestions {
		marker := "  "
		if index == m.suggestIndex {
			marker = "> "
		}
		label := "/" + spec.Name
		if spec.Args != "" {
			label += " " + spec.Args
		}
		line := marker + label + "  " + spec.Desc
		if index == m.suggestIndex {
			lines = append(lines, paintAccent(fitLine(line, width)))
		} else {
			lines = append(lines, paintMuted(fitLine(line, width)))
		}
	}
	return lines
}
