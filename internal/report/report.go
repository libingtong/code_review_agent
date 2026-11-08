// Package report owns the durable audit report format so both the TUI and the
// task orchestrator produce byte-identical output without importing each other.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/tools"
)

// Files keeps the historical reviewed/unreviewed split; both slices are always
// non-nil so downstream consumers never have to special-case null.
type Files struct {
	Reviewed   []tools.FileReview `json:"reviewed"`
	Unreviewed []tools.FileReview `json:"unreviewed"`
}

// TaskInfo is optional orchestrator context. It is omitted for single audits so
// existing consumers see exactly the same document as before.
type TaskInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Dir   string `json:"dir"`
}

type Report struct {
	GeneratedAt string                    `json:"generated_at"`
	Audit       tools.AuditState          `json:"audit"`
	Count       int                       `json:"count"`
	Findings    []tools.Finding           `json:"findings"`
	Revocations []agent.FindingRevocation `json:"revocations"`
	Todos       []tools.Todo              `json:"todos"`
	Files       Files                     `json:"files"`
	Variables   []tools.VariableReview    `json:"variables"`
	Flows       []tools.FlowReview        `json:"flows"`
	Project     tools.ProjectNote         `json:"project_note"`
	Task        *TaskInfo                 `json:"task,omitempty"`
}

// conclusionText falls back to a generated summary when the audit never closed
// (infinite mode has no end_audit tool), so a delivered report always states what
// was actually found instead of "暂无结论".
func (r Report) conclusionText() string {
	if strings.TrimSpace(r.Audit.Summary) != "" {
		return r.Audit.Summary
	}
	if len(r.Findings) == 0 {
		return `本轮未提交任何漏洞；已审文件数与覆盖情况见下方统计，不构成「无漏洞」结论。`
	}
	bySeverity := map[string]int{}
	for _, finding := range r.Findings {
		severity := strings.ToLower(strings.TrimSpace(finding.Severity))
		if severity == "" {
			severity = "unknown"
		}
		bySeverity[severity]++
	}
	parts := make([]string, 0, len(bySeverity))
	for _, severity := range []string{"critical", "high", "medium", "low", "info", "unknown"} {
		if count := bySeverity[severity]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", severity, count))
			delete(bySeverity, severity)
		}
	}
	head := fmt.Sprintf("本轮共提交 %d 个漏洞（%s）。审计未走完关闭流程，以下结论基于已完成证据：",
		len(r.Findings), strings.Join(parts, "、"))
	lines := []string{head}
	limit := len(r.Findings)
	if limit > 5 {
		limit = 5
	}
	for index := 0; index < limit; index++ {
		finding := r.Findings[index]
		location := finding.Path
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", finding.Path, finding.Line)
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s（%s）", finding.Severity, finding.Title, location))
	}
	if len(r.Findings) > limit {
		lines = append(lines, fmt.Sprintf("- 其余 %d 个见下方漏洞清单", len(r.Findings)-limit))
	}
	return strings.Join(lines, "\n")
}

// Build preserves every field the previous TUI-only exporter emitted. UI
// truncation is never applied: the report is the full evidence record.
func Build(snapshot tools.Snapshot, revocations []agent.FindingRevocation) Report {
	files := Files{Reviewed: []tools.FileReview{}, Unreviewed: []tools.FileReview{}}
	for _, f := range snapshot.Files {
		if f.Status == "reviewed" || f.Status == "skipped" {
			files.Reviewed = append(files.Reviewed, f)
		} else {
			files.Unreviewed = append(files.Unreviewed, f)
		}
	}
	findings := snapshot.Findings
	if findings == nil {
		findings = []tools.Finding{}
	}
	todos := snapshot.Todos
	if todos == nil {
		todos = []tools.Todo{}
	}
	variables := snapshot.Variables
	if variables == nil {
		variables = []tools.VariableReview{}
	}
	flows := snapshot.Flows
	if flows == nil {
		flows = []tools.FlowReview{}
	}
	revoked := revocations
	if revoked == nil {
		revoked = []agent.FindingRevocation{}
	}
	return Report{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Audit:       snapshot.Audit,
		Count:       len(findings),
		Findings:    findings,
		Revocations: revoked,
		Todos:       todos,
		Files:       files,
		Variables:   variables,
		Flows:       flows,
		Project:     snapshot.Project,
	}
}

func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func WriteJSON(path string, r Report) error {
	data, err := r.JSON()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// WriteMarkdown is a human-readable companion file for task orchestration.
// It deliberately summarises rather than re-listing every evidence byte.
func WriteMarkdown(path string, r Report) error {
	if err := os.WriteFile(path, []byte(r.Markdown()), 0600); err != nil {
		return err
	}
	return nil
}

func (r Report) Markdown() string {
	var b strings.Builder
	title := "代码安全审计报告"
	if r.Task != nil && r.Task.Title != "" {
		title += " · " + r.Task.Title
	}
	b.WriteString("# " + title + "\n\n")
	b.WriteString("- 生成时间：" + r.GeneratedAt + "\n")
	if r.Task != nil {
		b.WriteString("- 任务：" + r.Task.ID + " · " + r.Task.Dir + "\n")
	}
	b.WriteString(fmt.Sprintf("- 漏洞 %d · 文件 %d（已审 %d / 未审 %d）· Todo %d · 变量 %d · 链路 %d · 撤销 %d\n",
		r.Count, len(r.Files.Reviewed)+len(r.Files.Unreviewed), len(r.Files.Reviewed), len(r.Files.Unreviewed),
		len(r.Todos), len(r.Variables), len(r.Flows), len(r.Revocations)))
	if r.Audit.Ended {
		b.WriteString("- 审计结束状态：已结束\n")
	} else {
		b.WriteString("- 审计结束状态：未结束（覆盖可能不完整）\n")
	}
	b.WriteString("\n## 结论\n\n")
	if summary := r.conclusionText(); summary != "" {
		b.WriteString(summary + "\n")
	} else {
		b.WriteString("暂无结论（本轮未形成最终结论；覆盖情况见下）。\n")
	}
	if strings.TrimSpace(r.Audit.NextSteps) != "" {
		b.WriteString("\n## 下一步\n\n" + r.Audit.NextSteps + "\n")
	}

	b.WriteString("\n## 漏洞清单\n\n")
	if len(r.Findings) == 0 {
		b.WriteString("暂无已提交漏洞。\n")
	} else {
		findings := append([]tools.Finding(nil), r.Findings...)
		sort.SliceStable(findings, func(i, j int) bool {
			if severityRank(findings[i].Severity) != severityRank(findings[j].Severity) {
				return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
			}
			if findings[i].Path != findings[j].Path {
				return findings[i].Path < findings[j].Path
			}
			return findings[i].Line < findings[j].Line
		})
		b.WriteString("| # | 等级 | 标题 | 位置 | CWE |\n| --- | --- | --- | --- | --- |\n")
		for _, f := range findings {
			b.WriteString(fmt.Sprintf("| %d | %s | %s | %s:%d | %s |\n",
				f.ID, cell(f.Severity), cell(f.Title), cell(f.Path), f.Line, cell(f.CWE)))
		}
		for _, f := range findings {
			b.WriteString(fmt.Sprintf("\n### #%d [%s] %s\n\n- 位置：`%s:%d`\n", f.ID, f.Severity, f.Title, f.Path, f.Line))
			if f.CWE != "" {
				b.WriteString("- CWE：" + f.CWE + "\n")
			}
			if f.Evidence != "" {
				b.WriteString("- 证据：" + strings.TrimSpace(f.Evidence) + "\n")
			}
			if f.Impact != "" {
				b.WriteString("- 影响：" + strings.TrimSpace(f.Impact) + "\n")
			}
			if f.Recommendation != "" {
				b.WriteString("- 建议：" + strings.TrimSpace(f.Recommendation) + "\n")
			}
		}
	}

	if len(r.Revocations) > 0 {
		b.WriteString("\n## 已撤销结论\n\n")
		for _, rev := range r.Revocations {
			b.WriteString(fmt.Sprintf("- #%d %s · 原因：%s\n", rev.Finding.ID, cell(rev.Finding.Title), cell(rev.Reason)))
		}
	}

	b.WriteString("\n## 覆盖\n\n")
	b.WriteString(fmt.Sprintf("- 已审 %d 个文件，未覆盖 %d 个。\n", len(r.Files.Reviewed), len(r.Files.Unreviewed)))
	if len(r.Files.Unreviewed) > 0 {
		b.WriteString("\n未覆盖文件（前 50 个）：\n\n")
		for i, f := range r.Files.Unreviewed {
			if i == 50 {
				b.WriteString(fmt.Sprintf("- ……另有 %d 个\n", len(r.Files.Unreviewed)-50))
				break
			}
			b.WriteString("- `" + f.Path + "`\n")
		}
	}
	if strings.TrimSpace(r.Project.Note) != "" {
		b.WriteString("\n## 项目笔记\n\n" + r.Project.Note + "\n")
	}
	return b.String()
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "critical ", "严重", "致命":
		return 0
	case "high", "高", "高危":
		return 1
	case "medium", "中", "中危":
		return 2
	case "low", "低", "低危":
		return 3
	}
	return 4
}

func cell(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	return strings.ReplaceAll(text, "|", "\\|")
}
