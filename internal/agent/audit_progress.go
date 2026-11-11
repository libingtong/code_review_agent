package agent

import (
	"encoding/json"
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

// Paging a large tool result is how a worker reads evidence beyond the first
// page, and each page is new material rather than a repeated read. Credit is
// capped so a worker cannot page forever instead of concluding.
const auditProgressPageCredits = 24

// Reading material the worker has never seen (a new file, a new search result) is
// coverage progress even before it is written up as a conclusion. The credit is
// capped: after that the worker owes conclusions, and unchanged repeats earn
// nothing because the duplicate-read guard already withholds them.
const auditProgressEvidenceCredits = 8

type auditProgressGuard struct {
	turns           int
	warningTurns    int
	limitTurns      int
	known           map[string]bool
	pageOffsets     map[string]int64
	pageCredits     int
	evidenceCredits int
}

func newAuditProgressGuard(snapshot tools.Snapshot, cfg config.AgentConfig) *auditProgressGuard {
	warning, limit := cfg.AuditProgressTurns()
	g := &auditProgressGuard{known: make(map[string]bool), pageOffsets: make(map[string]int64), warningTurns: warning, limitTurns: limit}
	g.capture(snapshot)
	return g
}

// creditPage records that a read_tool_buffer call advanced to a new offset of
// its buffer. Only a successful page that starts beyond every already credited
// offset counts; the final (EOF) page does not, because after it the worker owes
// a conclusion rather than more paging.
func (g *auditProgressGuard) creditPage(tool, result string) bool {
	if tool != "read_tool_buffer" {
		return false
	}
	var page struct {
		OK         bool   `json:"ok"`
		BufferID   string `json:"buffer_id"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"next_offset"`
		EOF        bool   `json:"eof"`
	}
	if err := json.Unmarshal([]byte(result), &page); err != nil {
		return false
	}
	if !page.OK || page.BufferID == "" || page.EOF || page.NextOffset <= page.Offset {
		return false
	}
	if last, seen := g.pageOffsets[page.BufferID]; seen && page.Offset <= last {
		return false
	}
	if g.pageCredits >= auditProgressPageCredits {
		return false
	}
	g.pageOffsets[page.BufferID] = page.Offset
	g.pageCredits++
	g.turns = 0
	return true
}

// creditEvidence records that a tool call returned material this worker had not
// read before. It has to be a successful call that the duplicate-read guard saw as
// new; the cap keeps a long read-only sweep from substituting for conclusions.
func (g *auditProgressGuard) creditEvidence() bool {
	if g.evidenceCredits >= auditProgressEvidenceCredits {
		return false
	}
	g.evidenceCredits++
	g.turns = 0
	return true
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

// converging reports that the worker has spent its evidence-gathering window and
// now owes a written conclusion. Reading is still possible afterwards: a new
// conclusion resets the window.
func (g *auditProgressGuard) converging() bool {
	return g.turns >= g.warningTurns
}

func (g *auditProgressGuard) failure() error {
	return fmt.Errorf("审计停滞：连续 %d 回合没有新增文件/变量/链路结论或漏洞，已停止该成员以限制重复读取消耗；状态已保留，检查原因后可显式继续", g.turns)
}
