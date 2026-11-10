package agent

import (
	"fmt"
	"strconv"
	"strings"

	"code-review-agent/internal/config"
	"code-review-agent/internal/tools"
)

// An audit can be unbounded in duration without allowing an unbounded sequence
// of reads that never produces a conclusion. No status is changed by this guard.
const auditProgressWarningTurns = 8
const auditProgressLimitTurns = 16

type auditProgressGuard struct {
	turns        int
	warningTurns int
	limitTurns   int
	known        map[string]bool
}

func newAuditProgressGuard(snapshot tools.Snapshot, cfg config.AgentConfig) *auditProgressGuard {
	warning, limit := cfg.AuditProgressTurns()
	g := &auditProgressGuard{known: make(map[string]bool), warningTurns: warning, limitTurns: limit}
	g.capture(snapshot)
	return g
}

func (g *auditProgressGuard) capture(snapshot tools.Snapshot) bool {
	changed := false
	add := func(key string) {
		if !g.known[key] {
			g.known[key] = true
			changed = true
		}
	}
	for _, file := range snapshot.Files {
		if file.Status == "reviewed" || file.Status == "skipped" {
			add("file:" + file.Path)
		}
	}
	for _, finding := range snapshot.Findings {
		key := finding.Key
		if key == "" {
			key = finding.Path + ":" + strconv.Itoa(finding.Line) + ":" + finding.Title
		}
		add("finding:" + key)
	}
	// A recorded negative result on a cross-file flow or variable is a conclusion
	// too, even when it does not yield a finding or finish the containing file.
	for _, flow := range snapshot.Flows {
		if reviewConclusion(flow.Status) {
			add("flow:" + flow.Name)
		}
	}
	for _, variable := range snapshot.Variables {
		if reviewConclusion(variable.Status) {
			add("variable:" + variable.Path + ":" + variable.Name)
		}
	}
	return changed
}

func reviewConclusion(status string) bool {
	switch strings.ToLower(status) {
	case "reviewed", "benign", "done", "closed", "safe", "vulnerable", "excluded", "confirmed", "rejected", "skipped":
		return true
	}
	return false
}

func (g *auditProgressGuard) observe(snapshot tools.Snapshot) (warning string, err error) {
	if g.capture(snapshot) {
		g.turns = 0
		return "", nil
	}
	g.turns++
	if g.turns >= g.limitTurns {
		return "", g.failure()
	}
	if g.turns == g.warningTurns {
		return fmt.Sprintf("连续 %d 个审计回合没有新增审阅结论。请收敛当前证据，给出有依据的 file_review_update 结论或变量/链路结论，发现问题则 report_finding；重复读取、列目录和修改 reviewing 笔记不算完成。达到 %d 回合将暂停该成员并保留进度，不会自动标记文件完成。", g.warningTurns, g.limitTurns), nil
	}
	return "", nil
}

func (g *auditProgressGuard) failure() error {
	return fmt.Errorf("审计停滞：连续 %d 回合没有新增文件/变量/链路结论或漏洞，已停止该成员以限制重复读取消耗；状态已保留，检查原因后可显式继续", g.turns)
}
