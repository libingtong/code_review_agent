package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/tools"
)

func sampleSnapshot() tools.Snapshot {
	return tools.Snapshot{
		Todos:    []tools.Todo{{ID: 1, Title: "检查登录", Status: "done", Priority: "high"}},
		Findings: []tools.Finding{{ID: 1, Severity: "high", Title: "SQL 注入", Path: "a.go", Line: 42, Evidence: "拼接 SQL", CWE: "CWE-89"}},
		Files: []tools.FileReview{
			{Path: "a.go", Status: "reviewed", Note: "已读"},
			{Path: "b.go", Status: "unseen"},
			{Path: "c.go", Status: "skipped"},
		},
		Variables: []tools.VariableReview{{Name: "token"}},
		Flows:     []tools.FlowReview{{Name: "登录链路"}},
		Project:   tools.ProjectNote{Note: "Go 服务"},
		Audit:     tools.AuditState{Ended: true, Summary: "整体风险偏高", NextSteps: "补审 b.go"},
	}
}

// The report format is consumed by external tooling; keep the exact key set.
func TestBuildKeepsHistoricalJSONShape(t *testing.T) {
	data, err := Build(sampleSnapshot(), nil).JSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"audit", "count", "files", "findings", "generated_at", "project_note", "revocations", "todos", "variables", "flows"}
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("report keys drifted:\n got %v\nwant %v", keys, want)
	}
	if string(decoded["generated_at"]) == "" || string(decoded["generated_at"]) == `""` {
		t.Fatal("generated_at must be populated")
	}
}

func TestBuildSplitsFilesAndNeverEmitsNull(t *testing.T) {
	built := Build(sampleSnapshot(), nil)
	if built.Count != 1 {
		t.Fatalf("count = %d, want 1", built.Count)
	}
	if len(built.Files.Reviewed) != 2 || len(built.Files.Unreviewed) != 1 {
		t.Fatalf("file split wrong: reviewed=%d unreviewed=%d", len(built.Files.Reviewed), len(built.Files.Unreviewed))
	}
	empty := Build(tools.Snapshot{}, nil)
	data, err := empty.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"null"} {
		if strings.Contains(string(data), needle) {
			t.Fatalf("empty report must not contain %q:\n%s", needle, data)
		}
	}
	if empty.Task != nil {
		t.Fatal("task context must be omitted for single audits")
	}
}

func TestWriteBothFormats(t *testing.T) {
	dir := t.TempDir()
	built := Build(sampleSnapshot(), []agent.FindingRevocation{{Key: "k", Reason: "误报", Evidence: "重读代码", Finding: tools.Finding{ID: 7, Title: "旧结论"}}})
	built.Task = &TaskInfo{ID: "T001", Title: "示例任务", Dir: "/tmp/repo"}
	jsonPath := filepath.Join(dir, "report.json")
	mdPath := filepath.Join(dir, "report.md")
	if err := WriteJSON(jsonPath, built); err != nil {
		t.Fatal(err)
	}
	if err := WriteMarkdown(mdPath, built); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(md)
	for _, needle := range []string{"# 代码安全审计报告 · 示例任务", "SQL 注入", "已审 2 个文件", "旧结论", "b.go"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("markdown missing %q:\n%s", needle, text)
		}
	}
	if info, err := os.Stat(jsonPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("json report must be 0600: %v %v", info, err)
	}
}

func TestSeverityOrderAndEscaping(t *testing.T) {
	built := Build(tools.Snapshot{Findings: []tools.Finding{
		{ID: 1, Severity: "low", Title: "a|b"},
		{ID: 2, Severity: "critical", Title: "boom"},
	}}, nil)
	text := built.Markdown()
	if strings.Index(text, "boom") > strings.Index(text, "a\\|b") {
		t.Fatal("critical findings must be listed before low ones")
	}
	if !strings.Contains(text, `a\|b`) {
		t.Fatal("pipe characters must be escaped in markdown tables")
	}
}

// A run that never closed (infinite mode has no end_audit) must still summarise
// what it found, instead of emitting "暂无结论".
func TestReportConclusionFallsBackToFindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	built := Report{GeneratedAt: "2026-11-08T00:00:00Z", Findings: []tools.Finding{
		{ID: 1, Severity: "high", Title: "硬编码私钥", Path: "a/b.java", Line: 40},
		{ID: 2, Severity: "medium", Title: "回调未验签", Path: "c/d.java", Line: 113},
	}}
	if err := WriteMarkdown(path, built); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "暂无结论（本轮未形成最终结论") && !strings.Contains(text, "本轮共提交 2 个漏洞") {
		t.Fatalf("conclusion not synthesised:\n%s", text)
	}
	if !strings.Contains(text, "本轮共提交 2 个漏洞") || !strings.Contains(text, "high 1") {
		t.Fatalf("conclusion text wrong:\n%s", text)
	}
}
