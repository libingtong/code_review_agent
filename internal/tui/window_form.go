package tui

import (
	"fmt"
	"strings"

	"code-review-agent/internal/config"
	"code-review-agent/internal/schedule"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// windowModal edits the working-hours policy: a list of windows plus an inline
// form. Every change applies to the live gate immediately and is persisted to
// schedule.store_file, so it survives a restart.
type windowModal struct {
	enabled  bool
	selected int
	editing  bool
	fields   [3]textinput.Model
	timezone string
	store    string
	status   string
}

func newWindowModal(cfg config.ScheduleConfig, current *schedule.Schedule) *windowModal {
	form := &windowModal{timezone: cfg.Timezone, store: cfg.StoreFile}
	if form.timezone == "" {
		form.timezone = "Asia/Shanghai"
	}
	if form.store == "" {
		form.store = "state/schedule.json"
	}
	if current != nil {
		form.enabled = current.Enabled
	}
	defaults := [3]string{"21:00", "08:00", "all"}
	for i := range defaults {
		input := textinput.New()
		input.Prompt = ""
		input.CharLimit = 40
		input.SetValue(defaults[i])
		form.fields[i] = input
	}
	return form
}

func (m *Model) openWindowPrompt() tea.Cmd {
	if m.gate == nil {
		m.addEvent("时段门控未初始化。")
		return nil
	}
	state := newWindowModal(m.cfg.Schedule, m.gate.CurrentSchedule())
	m.modal = &modalState{title: "工作时间段 · /window", window: state}
	m.input.Blur()
	m.resizeModal()
	return nil
}

// windowsOf snapshots the live policy for editing.
func (m *Model) windowsOf() []schedule.Window {
	if m.gate == nil {
		return nil
	}
	current := m.gate.CurrentSchedule()
	if current == nil {
		return nil
	}
	return append([]schedule.Window(nil), current.Windows...)
}

// applyWindows rebuilds the global policy from edited windows and persists the
// whole plan (global + channel overrides) so nothing is lost.
func (m *Model) applyWindows(windows []schedule.Window, enabled bool) error {
	policy := schedule.Policy{Enabled: enabled, Timezone: m.modal.window.timezone}
	for _, window := range windows {
		policy.Windows = append(policy.Windows, schedule.WindowSpec{
			Start: window.ClockStart(), End: window.ClockEnd(), Days: window.DayNames(),
		})
	}
	next, err := schedule.NewFromPolicy(policy)
	if err != nil {
		return err
	}
	m.gate.Plan().SetGlobal(next)
	m.cfg.Schedule.Enabled = enabled
	return m.savePlan()
}

// savePlan persists the live two-level policy.
func (m *Model) savePlan() error {
	path := m.cfg.Schedule.StoreFile
	if path == "" {
		path = "state/schedule.json"
	}
	if err := schedule.SavePolicy(path, m.gate.Plan().PolicyOf()); err != nil {
		return fmt.Errorf("已应用，但保存失败：%w", err)
	}
	return nil
}

// applyChannel sets (or clears, with an empty override) one channel's policy.
func (m *Model) applyChannel(id string, override *schedule.Policy) error {
	if override == nil {
		m.gate.Plan().SetChannel(id, nil)
		return m.savePlan()
	}
	next, err := schedule.NewFromPolicy(*override)
	if err != nil {
		return err
	}
	m.gate.Plan().SetChannel(id, next)
	return m.savePlan()
}

func (m *Model) updateWindow(msg tea.Msg) tea.Cmd {
	form := m.modal.window
	if form.editing {
		return m.updateWindowEditor(msg, form)
	}
	windows := m.windowsOf()
	// Clamp before handling keys: a freshly added window leaves selected at -1,
	// and using an out-of-range index would silently no-op edit/delete.
	if len(windows) > 0 {
		form.selected = min(max(0, form.selected), len(windows)-1)
	} else {
		form.selected = 0
	}
	if key, isKey := msg.(tea.KeyMsg); isKey {
		switch key.String() {
		case "esc", "ctrl+[":
			return m.closeModal()
		case "ctrl+c":
			cmd, _ := m.handleKey(key)
			return cmd
		case "up", "k":
			form.selected--
		case "down", "j":
			form.selected++
		case " ", "space":
			if err := m.applyWindows(windows, !form.enabled); err != nil {
				form.status = err.Error()
				break
			}
			form.enabled = !form.enabled
			if form.enabled {
				form.status = "时段限制已开启"
			} else {
				form.status = "时段限制已关闭（任何时间都可执行）"
			}
		case "a", "n":
			return m.startWindowEdit(-1)
		case "enter", "e":
			if len(windows) == 0 {
				return m.startWindowEdit(-1)
			}
			return m.startWindowEdit(form.selected)
		case "d":
			if len(windows) == 0 {
				break
			}
			remaining := removeWindow(windows, form.selected)
			// Removing the last window would otherwise mean "always paused"; turn
			// the policy off instead so work is never blocked invisibly.
			enabled := form.enabled && len(remaining) > 0
			if err := m.applyWindows(remaining, enabled); err != nil {
				form.status = err.Error()
				break
			}
			form.enabled = enabled
			if len(remaining) == 0 {
				form.status = "已删除最后一个窗口，时段限制自动关闭"
			} else {
				form.status = "已删除窗口并保存"
			}
		}
	}
	if mouse, ok := msg.(tea.MouseMsg); ok {
		switch mouse.Type {
		case tea.MouseWheelUp:
			form.selected--
		case tea.MouseWheelDown:
			form.selected++
		}
	}
	if count := len(windows); count > 0 {
		form.selected = min(max(0, form.selected), count-1)
	}
	m.modal.wrapWidth = 0
	m.resizeModal()
	return nil
}

func removeWindow(windows []schedule.Window, index int) []schedule.Window {
	if index < 0 || index >= len(windows) {
		return windows
	}
	out := make([]schedule.Window, 0, len(windows)-1)
	out = append(out, windows[:index]...)
	out = append(out, windows[index+1:]...)
	return out
}

// startWindowEdit opens the form for an existing window (-1 adds a new one).
func (m *Model) startWindowEdit(index int) tea.Cmd {
	form := m.modal.window
	windows := m.windowsOf()
	values := [3]string{"21:00", "08:00", "all"}
	if index >= 0 && index < len(windows) {
		window := windows[index]
		values = [3]string{window.ClockStart(), window.ClockEnd(), window.DescribeDays()}
		form.selected = index
	} else {
		form.selected = -1
	}
	for i := range form.fields {
		form.fields[i].SetValue(values[i])
	}
	form.editing = true
	form.status = ""
	m.modal.err = ""
	for i := range form.fields {
		form.fields[i].Blur()
	}
	cmd := form.fields[0].Focus()
	m.resizeModal()
	return cmd
}

func (m *Model) updateWindowEditor(msg tea.Msg, form *windowModal) tea.Cmd {
	if key, isKey := msg.(tea.KeyMsg); isKey {
		switch key.String() {
		case "esc", "ctrl+[":
			return m.leaveWindowEditor(form, "已取消编辑")
		case "ctrl+c":
			cmd, _ := m.handleKey(key)
			return cmd
		case "tab", "down":
			return m.focusWindowField(1)
		case "shift+tab", "up":
			return m.focusWindowField(-1)
		case "ctrl+s":
			return m.saveWindowEditor(form)
		}
	}
	if _, mouse := msg.(tea.MouseMsg); mouse {
		return nil
	}
	focused := -1
	for i := range form.fields {
		if form.fields[i].Focused() {
			focused = i
			break
		}
	}
	if focused < 0 {
		return nil
	}
	before := form.fields[focused].Value()
	var cmd tea.Cmd
	form.fields[focused], cmd = form.fields[focused].Update(msg)
	if before != form.fields[focused].Value() {
		m.modal.err = ""
	}
	return cmd
}

func (m *Model) saveWindowEditor(form *windowModal) tea.Cmd {
	start := strings.TrimSpace(form.fields[0].Value())
	end := strings.TrimSpace(form.fields[1].Value())
	days, err := schedule.ParseDays(form.fields[2].Value())
	if err != nil {
		m.modal.err = err.Error()
		return nil
	}
	spec := schedule.Policy{Enabled: true, Timezone: form.timezone,
		Windows: []schedule.WindowSpec{{Start: start, End: end, Days: days}}}
	probe, err := schedule.NewFromPolicy(spec)
	if err != nil {
		m.modal.err = err.Error()
		return nil
	}
	windows := m.windowsOf()
	updated := probe.Windows[0]
	if form.selected >= 0 && form.selected < len(windows) {
		windows[form.selected] = updated
	} else {
		windows = append(windows, updated)
	}
	if err := m.applyWindows(windows, true); err != nil {
		m.modal.err = err.Error()
		return nil
	}
	form.enabled = true
	message := updated.ClockStart() + "-" + updated.ClockEnd() + " " + updated.DescribeDays()
	m.addEvent("工作时间段已更新：" + message)
	m.syncTeam()
	return m.leaveWindowEditor(form, "已保存："+message)
}

func (m *Model) leaveWindowEditor(form *windowModal, status string) tea.Cmd {
	form.editing = false
	form.status = status
	for i := range form.fields {
		form.fields[i].Blur()
	}
	m.modal.wrapWidth = 0
	m.resizeModal()
	return nil
}

func (m *Model) focusWindowField(step int) tea.Cmd {
	form := m.modal.window
	current := 0
	for i := range form.fields {
		if form.fields[i].Focused() {
			current = i
			break
		}
	}
	next := (current + step + len(form.fields)) % len(form.fields)
	for i := range form.fields {
		form.fields[i].Blur()
	}
	return form.fields[next].Focus()
}

func (m Model) windowLines(width, height int) []string {
	form := m.modal.window
	if form.editing {
		return m.windowEditorLines(width, height)
	}
	state := "关闭"
	if form.enabled {
		state = "开启"
	}
	lines := []string{
		paintMuted(fitLine("时段限制 ["+state+"] · 窗口外暂停，到点自动恢复；跨午夜把开始设得比结束晚", width)),
		"",
	}
	windows := m.windowsOf()
	if len(windows) == 0 {
		lines = append(lines, "  还没有窗口；按 a 新增，例如 21:00-08:00 每天")
	}
	for i, window := range windows {
		marker := "  "
		if i == form.selected {
			marker = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%s-%s · %s", marker, window.ClockStart(), window.ClockEnd(), window.DescribeDays()))
	}
	lines = append(lines, "", "a 新增 · Enter/e 编辑 · d 删除 · 空格 开关 · Esc 关闭")
	if form.status != "" {
		lines = append(lines, paintAccent(fitLine(form.status, width)))
	}
	return viewport(lines, 0, height, false)
}

func (m Model) windowEditorLines(width, height int) []string {
	form := m.modal.window
	action := "新增窗口"
	if form.selected >= 0 && form.selected < len(m.windowsOf()) {
		action = fmt.Sprintf("编辑窗口 #%d", form.selected+1)
	}
	lines := []string{paintMuted(fitLine(action+" · 时区 "+form.timezone, width)), ""}
	labels := [3]string{"开始", "结束", "星期"}
	for i, label := range labels {
		lines = append(lines, paintLine(fitLine("  "+label, width)))
		lines = append(lines, "  "+form.fields[i].View(), "")
	}
	for _, text := range []string{
		"星期可写 all / weekdays / weekend / mon-fri / mon,wed,fri",
		"跨午夜示例：21:00 到 08:00（窗口属于开始那天，周五 22:00 覆盖到周六 06:00）",
		"Ctrl+S 保存并立即生效，写入 " + storeLabel(form.store) + "，重启后仍是这个设置",
	} {
		for _, line := range wrapLines(text, width) {
			lines = append(lines, paintMuted(line))
		}
	}
	return viewport(lines, 0, height, false)
}

// windowCommand handles the scriptable forms: /window off|on|clear|<start>-<end>.
func (m *Model) windowCommand(arg string) tea.Cmd {
	if m.gate == nil {
		m.addEvent("时段门控未初始化。")
		return nil
	}
	value := strings.ToLower(strings.TrimSpace(arg))
	// "/window <channel> always|inherit|HH:MM-HH:MM [days]" overrides one channel.
	if fields := strings.Fields(value); len(fields) >= 2 && m.isChannel(fields[0]) {
		return m.channelWindowCommand(fields[0], strings.Join(fields[1:], " "))
	}
	switch value {
	case "off", "disable", "关":
		if err := m.applyPolicy(schedule.Policy{Enabled: false, Timezone: timezoneOf(m)}); err != nil {
			m.addEvent("关闭时段失败：" + err.Error())
			return nil
		}
		m.addEvent("工作时间段已关闭：任何时间都可执行")
		m.syncTeam()
		return nil
	case "on", "enable", "开":
		if err := m.applyPolicy(schedule.Policy{Enabled: true, Timezone: timezoneOf(m),
			Windows: []schedule.WindowSpec{{Start: "21:00", End: "08:00", Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}}}}); err != nil {
			m.addEvent("开启时段失败：" + err.Error())
			return nil
		}
		m.addEvent("工作时间段已开启：21:00-08:00 每天（/window 可改）")
		m.syncTeam()
		return nil
	}
	// "/window 21:00-08:00 [mon-fri]" is the terse path.
	parts := strings.Fields(value)
	clockRange := parts[0]
	days := "all"
	if len(parts) > 1 {
		days = strings.Join(parts[1:], "")
	}
	start, end, ok := strings.Cut(clockRange, "-")
	if !ok {
		m.addEvent("用法：/window 或 /window off|on|HH:MM-HH:MM [all|weekdays|mon-fri]")
		return nil
	}
	dayList, err := schedule.ParseDays(days)
	if err != nil {
		m.addEvent("星期写法无效：" + err.Error())
		return nil
	}
	if err := m.applyPolicy(schedule.Policy{Enabled: true, Timezone: timezoneOf(m),
		Windows: []schedule.WindowSpec{{Start: start, End: end, Days: dayList}}}); err != nil {
		m.addEvent("设置时段失败：" + err.Error())
		return nil
	}
	m.addEvent("工作时间段已更新：" + start + "-" + end + " " + days)
	m.syncTeam()
	return nil
}

// applyPolicy replaces only the global policy used by scriptable commands.
func (m *Model) applyPolicy(policy schedule.Policy) error {
	next, err := schedule.NewFromPolicy(policy)
	if err != nil {
		return err
	}
	m.gate.Plan().SetGlobal(next)
	m.cfg.Schedule.Enabled = policy.Enabled
	return m.savePlan()
}

// isChannel reports whether id names a configured LLM source.
func (m *Model) isChannel(id string) bool {
	if m.pool == nil {
		return false
	}
	for _, status := range m.pool.Statuses() {
		if status.ID == id {
			return true
		}
	}
	return false
}

// channelWindowCommand handles per-channel overrides:
//
//	/window glm always            渠道全天可用（忽略全局时段）
//	/window glm inherit           恢复跟随全局
//	/window glm 21:00-02:00 all   渠道自己的时段
func (m *Model) channelWindowCommand(id, arg string) tea.Cmd {
	value := strings.ToLower(strings.TrimSpace(arg))
	switch value {
	case "always", "on", "all-day", "全天":
		if err := m.applyChannel(id, &schedule.Policy{Enabled: false, Timezone: timezoneOf(m)}); err != nil {
			m.addEvent("设置渠道时段失败：" + err.Error())
			return nil
		}
		m.addEvent("渠道 " + id + " 已设为全天可用（忽略全局时段）")
		m.syncTeam()
		return nil
	case "inherit", "global", "跟随":
		if err := m.applyChannel(id, nil); err != nil {
			m.addEvent("设置渠道时段失败：" + err.Error())
			return nil
		}
		m.addEvent("渠道 " + id + " 已恢复跟随全局时段")
		m.syncTeam()
		return nil
	}
	parts := strings.Fields(value)
	start, end, ok := strings.Cut(parts[0], "-")
	if !ok {
		m.addEvent("用法：/window " + id + " always|inherit|HH:MM-HH:MM [all|weekdays|mon-fri]")
		return nil
	}
	days := "all"
	if len(parts) > 1 {
		days = strings.Join(parts[1:], "")
	}
	dayList, err := schedule.ParseDays(days)
	if err != nil {
		m.addEvent("星期写法无效：" + err.Error())
		return nil
	}
	if err := m.applyChannel(id, &schedule.Policy{Enabled: true, Timezone: timezoneOf(m),
		Windows: []schedule.WindowSpec{{Start: start, End: end, Days: dayList}}}); err != nil {
		m.addEvent("设置渠道时段失败：" + err.Error())
		return nil
	}
	m.addEvent("渠道 " + id + " 时段：" + start + "-" + end + " " + days)
	m.syncTeam()
	return nil
}

func timezoneOf(m *Model) string {
	if m.cfg.Schedule.Timezone != "" {
		return m.cfg.Schedule.Timezone
	}
	return "Asia/Shanghai"
}

func storeLabel(path string) string {
	if path == "" {
		return "内存（未配置持久化文件）"
	}
	return path
}
