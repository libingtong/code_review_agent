package tui

import (
	"fmt"
	"strings"

	"code-review-agent/internal/provider"
	tea "github.com/charmbracelet/bubbletea"
)

// providerModal is the interactive source list. It stays deliberately small:
// selection + three actions, all reachable without leaving the panel.
type providerModal struct {
	selected int
	status   string
}

func (m *Model) openProviders() tea.Cmd {
	if m.pool == nil {
		m.addEvent("当前没有多源配置；config.yaml 的 providers 段为空。")
		return nil
	}
	m.modal = &modalState{title: "LLM 源 · /providers", providers: &providerModal{}}
	m.modal.lines = m.providerLines()
	m.modal.wrapWidth = 0
	m.input.Blur()
	m.resizeModal()
	return nil
}

func (m *Model) providerLines() []string {
	if m.pool == nil {
		return []string{"没有源池"}
	}
	statuses := m.pool.Statuses()
	lines := []string{
		"↑↓ 选择 · Enter 强制切换 · e 启停 · r 刷新余量 · /limit <ID> <90%|off> 设自设上限 · Esc 关闭",
		"",
	}
	for index, status := range statuses {
		marker := "  "
		if index == m.modal.providers.selected {
			marker = "> "
		}
		active := "  "
		if status.Active {
			active = "▸ "
		}
		enabled := "启用"
		if !status.Enabled {
			enabled = "停用"
		}
		ratio := "余量未知"
		if status.Total > 0 {
			ratio = fmt.Sprintf("余量 %.0f%%（%.4g/%.4g %s）", status.Ratio*100, status.Remaining, status.Total, status.Unit)
		}
		reset := "无活动窗口"
		if !status.ResetAt.IsZero() {
			reset = "⟶ " + status.ResetAt.Local().Format("01-02 15:04")
		}
		channel := status.Vendor
		if status.Plan != "" {
			channel += " · " + status.Plan
		}
		if status.PlanObserved != "" && !strings.EqualFold(status.PlanObserved, strings.TrimPrefix(strings.ToLower(status.Plan), "glm-")) {
			// The vendor knows the real tier; trusting the config blindly would
			// report the wrong capacity.
			channel += "（接口报告：" + status.PlanObserved + "，配置可能不符）"
		}
		lines = append(lines, fmt.Sprintf("%s%s%s %s [%s] · 渠道 %s", marker, active, status.Label, status.ID, enabled, channel))
		lines = append(lines, "    "+ratio+" · "+reset+" · "+status.Model)
		if status.SelfLimited {
			lines = append(lines, fmt.Sprintf("    自设上限 %.0f%%（可用 %s / 上限 %s %s）· /limit %s <90%%|off> 调整",
				status.MaxUsageRatio*100, provider.FormatUnits(status.Usable), provider.FormatUnits(status.Ceiling), status.Unit, status.ID))
		} else {
			lines = append(lines, "    未设自设上限（可用全部额度）· /limit "+status.ID+" 90% 可留出余量给其它工具")
		}
		scheduled := "时段可用"
		if !status.Scheduled {
			scheduled = "时段外"
		}
		if m.gate != nil {
			policy := m.gate.Plan()
			if policy.Overridden(status.ID) {
				scheduled += "（渠道自有时段：" + policy.Channel(status.ID).Describe() + "）"
			} else {
				scheduled += "（跟随全局：" + policy.Global().Describe() + "）"
			}
		}
		lines = append(lines, "    "+scheduled)
		concurrency := fmt.Sprintf("    并发 %d/%d（自适应）· 在飞 %d · 429 %d 次",
			status.Concurrency, status.MaxConcur, status.InFlight, status.Throttles)
		if status.Throttles > 0 && status.Concurrency <= 2 {
			concurrency += " · 已压到很低，说明不是并发问题而是额度/套餐限制"
		}
		lines = append(lines, concurrency)
		lines = append(lines, "    来源 "+status.Source)
		if status.LastError != "" {
			lines = append(lines, "    "+boundedText("最近错误："+status.LastError, 400))
		}
		lines = append(lines, "")
	}
	if m.modal.providers.status != "" {
		lines = append(lines, m.modal.providers.status)
	}
	return lines
}

func (m *Model) refreshProvidersModal() {
	if m.modal == nil || m.modal.providers == nil || m.pool == nil {
		return
	}
	m.modal.lines = m.providerLines()
	m.modal.wrapWidth = 0
	m.resizeModal()
}

func (m *Model) updateProviders(msg tea.Msg) tea.Cmd {
	if m.modal == nil || m.modal.providers == nil || m.pool == nil {
		return nil
	}
	state := m.modal.providers
	statuses := m.pool.Statuses()
	if len(statuses) == 0 {
		return nil
	}
	switch key := msg.(type) {
	case tea.KeyMsg:
		switch key.String() {
		case "up", "k":
			state.selected--
		case "down", "j":
			state.selected++
		case "enter":
			status := statuses[min(max(0, state.selected), len(statuses)-1)]
			if err := m.pool.SetActive(status.ID); err != nil {
				state.status = "切换失败：" + err.Error()
			} else {
				m.addEvent("已强制切换到源 " + status.ID)
				state.status = "已切换到 " + status.ID
			}
		case "e":
			status := statuses[min(max(0, state.selected), len(statuses)-1)]
			if err := m.pool.SetEnabled(status.ID, !status.Enabled); err != nil {
				state.status = "启停失败：" + err.Error()
			} else {
				state.status = fmt.Sprintf("源 %s 已%s", status.ID, map[bool]string{true: "启用", false: "停用"}[!status.Enabled])
			}
		case "r":
			if m.quotaRefresh != nil {
				m.quotaRefresh()
			}
			state.status = "已请求刷新余量"
		}
	case tea.MouseMsg:
		if key.Type == tea.MouseWheelUp {
			state.selected--
		}
		if key.Type == tea.MouseWheelDown {
			state.selected++
		}
	}
	state.selected = min(max(0, state.selected), len(statuses)-1)
	m.refreshProvidersModal()
	m.syncTeam()
	return nil
}

// showQuota prints the quota table, and the raw vendor payload when a source is
// named. Raw output is what makes field-mapping drift fixable from the UI.
func (m *Model) showQuota(arg string) {
	if m.pool == nil {
		m.addEvent("当前没有多源配置。")
		return
	}
	id := strings.TrimSpace(arg)
	lines := []string{"5 小时窗口余量", ""}
	lines = append(lines, m.pool.QuotaLines()...)
	if id != "" {
		sample, raw, err := m.pool.RefreshQuota(m.context(), id)
		lines = append(lines, "", "== "+id+" 探针 ==")
		if err != nil {
			lines = append(lines, "错误："+err.Error())
		}
		if raw != "" {
			lines = append(lines, "原始响应：")
			for _, line := range wrapLines(raw, max(20, m.width-8)) {
				lines = append(lines, line)
			}
		}
		if sample.HasUsed || sample.HasTotal || sample.HasReset {
			lines = append(lines, fmt.Sprintf("解析：used=%.4g total=%.4g reset=%s",
				sample.Used, sample.Total, sample.ResetAt.Local().Format("01-02 15:04")))
		}
	}
	m.syncTeam()
	m.showDetail("余量 · /quota", lines)
}

// providerStatusLine is the compact header indicator.
func (m *Model) providerStatusLine() string {
	if m.pool == nil {
		return ""
	}
	summary := m.pool.QuotaSummary()
	if summary == "" {
		return ""
	}
	if m.gate != nil {
		return summary + " · 时段 " + m.gate.Describe()
	}
	return summary
}

// limitCommand implements /limit: show, set or clear a source's self-imposed
// usage cap so the rest of the subscription stays available to other tools.
func (m *Model) limitCommand(arg string) tea.Cmd {
	if m.pool == nil {
		m.addEvent("当前没有多源配置。")
		return nil
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		lines := []string{"自设用量上限：本程序最多用掉窗口的这一部分，其余留给其它调用同一订阅的系统。", ""}
		for _, status := range m.pool.Statuses() {
			if status.SelfLimited {
				lines = append(lines, fmt.Sprintf("%s（%s）：上限 %.0f%%，已用 %s / %s %s，可用 %s",
					status.ID, status.Plan, status.MaxUsageRatio*100, provider.FormatUnits(status.Used), provider.FormatUnits(status.Ceiling), status.Unit, provider.FormatUnits(status.Usable)))
			} else {
				lines = append(lines, fmt.Sprintf("%s（%s）：未设上限（可用全部 %s %s）", status.ID, status.Plan, provider.FormatUnits(status.Total), status.Unit))
			}
		}
		lines = append(lines, "", "设置：/limit glm 90%  清除：/limit glm off  保留绝对值：/limit glm reserve 500")
		m.showDetail("用量上限 · /limit", lines)
		return nil
	}
	id := fields[0]
	if len(fields) < 2 {
		m.addEvent("用法：/limit <ID> <90%|0.9|off>，或 /limit <ID> reserve <数量>")
		return nil
	}
	ratio, reserve, ok := m.pool.UsageLimit(id)
	if !ok {
		m.addEvent("未知的源：" + id)
		return nil
	}
	value := strings.ToLower(strings.TrimSpace(fields[1]))
	switch {
	case value == "off" || value == "none" || value == "0":
		ratio, reserve = 1, 0
	case value == "reserve":
		if len(fields) < 3 {
			m.addEvent("用法：/limit <ID> reserve <数量>")
			return nil
		}
		number, ok := parseNumber(fields[2])
		if !ok {
			m.addEvent("保留量必须是数字：" + fields[2])
			return nil
		}
		reserve = number
	default:
		number, ok := parsePercent(value)
		if !ok {
			m.addEvent("用量上限写成 90% 或 0.9：" + fields[1])
			return nil
		}
		ratio = number
	}
	if err := m.pool.SetUsageLimit(id, ratio, reserve); err != nil {
		m.addEvent("设置失败：" + err.Error())
		return nil
	}
	m.addEvent(fmt.Sprintf("源 %s 的用量上限已更新：上限 %.0f%%，保留 %s（已持久化）", id, ratio*100, provider.FormatUnits(reserve)))
	m.syncTeam()
	return nil
}

func parsePercent(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	percent := strings.HasSuffix(value, "%")
	value = strings.TrimSuffix(value, "%")
	number, ok := parseNumber(value)
	if !ok {
		return 0, false
	}
	if percent || number > 1 {
		number /= 100
	}
	if number <= 0 || number > 1 {
		return 0, false
	}
	return number, true
}

func parseNumber(value string) (float64, bool) {
	var number float64
	var dot, digits bool
	scale := 1.0
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= '0' && r <= '9':
			digits = true
			if dot {
				scale /= 10
				number += float64(r-'0') * scale
			} else {
				number = number*10 + float64(r-'0')
			}
		case r == '.' && !dot:
			dot = true
		default:
			return 0, false
		}
	}
	return number, digits
}
