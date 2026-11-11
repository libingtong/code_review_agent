package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func TestAuditProgressStopsRepeatedReadsWithoutManufacturingCompletion(t *testing.T) {
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, _ []llm.Message) (llm.ToolResponse, error) {
		calls++
		return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 10}), nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.cfg.Agent.InfiniteMode = true
	a.Run(context.Background(), "inspect", func(Event) {})
	if calls != auditProgressLimitTurns || a.runErr == nil || !strings.Contains(a.runErr.Error(), "审计停滞") || a.completed {
		t.Fatalf("calls=%d err=%v completed=%v", calls, a.runErr, a.completed)
	}
	for _, f := range a.tools.Snapshot().Files {
		if f.Status == "reviewed" {
			t.Fatal("guard invented a review conclusion")
		}
	}
	// An automatic moderator reactivation must not buy another progress window.
	a.Run(context.Background(), "continue", func(Event) {})
	if calls != auditProgressLimitTurns {
		t.Fatalf("reactivation reset guard: calls=%d", calls)
	}
}

func TestAuditProgressCustomWindowAndMessages(t *testing.T) {
	g := newAuditProgressGuard(tools.Snapshot{}, config.AgentConfig{AuditProgressWarningTurns: 2, AuditProgressLimitTurns: 4})
	for turn := 1; turn <= 4; turn++ {
		warning, err := g.observe(tools.Snapshot{})
		if turn == 2 && (!strings.Contains(warning, "连续 2 个") || !strings.Contains(warning, "达到 4 回合")) {
			t.Fatalf("custom warning: %s", warning)
		}
		if turn != 2 && warning != "" {
			t.Fatalf("unexpected warning at %d", turn)
		}
		if (err != nil) != (turn == 4) {
			t.Fatalf("custom stop at %d: %v", turn, err)
		}
	}
}

func TestAuditProgressOnlyNewConclusionsResetWindow(t *testing.T) {
	s := tools.Snapshot{Files: []tools.FileReview{{Path: "a.go", Status: "reviewing"}}}
	g := newAuditProgressGuard(s, config.AgentConfig{})
	for i := 0; i < auditProgressWarningTurns; i++ {
		warning, err := g.observe(s)
		if err != nil {
			t.Fatal(err)
		}
		if (warning != "") != (i == auditProgressWarningTurns-1) {
			t.Fatalf("warning at %d: %s", i, warning)
		}
	}
	s.Files[0].Note = "read once more"
	g.observe(s)
	if g.turns != auditProgressWarningTurns+1 {
		t.Fatal("note edit reset window")
	}
	s.Files[0].Status = "reviewed"
	g.observe(s)
	if g.turns != 0 {
		t.Fatal("file conclusion did not reset window")
	}
	s.Files[0].Status = "reviewing"
	g.observe(s)
	s.Files[0].Status = "reviewed"
	g.observe(s)
	if g.turns != 2 {
		t.Fatal("toggling old conclusion reset window")
	}
	s.Findings = []tools.Finding{{Key: "evidence-one"}}
	g.observe(s)
	if g.turns != 0 {
		t.Fatal("finding did not reset window")
	}
	g.observe(s)
	if g.turns != 1 {
		t.Fatal("same finding reset window")
	}
	s.Flows = []tools.FlowReview{{Name: "auth-flow", Status: "benign"}}
	g.observe(s)
	if g.turns != 0 {
		t.Fatal("negative flow conclusion did not reset window")
	}
	s.Variables = []tools.VariableReview{{Path: "a.go", Name: "id", Status: "reviewed"}}
	g.observe(s)
	if g.turns != 0 {
		t.Fatal("variable conclusion did not reset window")
	}
}

func TestAuditProgressCreditsGenuineBufferPagingButStaysBounded(t *testing.T) {
	g := newAuditProgressGuard(tools.Snapshot{}, config.AgentConfig{})
	page := func(offset int64, ok, eof bool) string {
		data, _ := json.Marshal(map[string]any{"ok": ok, "buffer_id": "buffer-1", "offset": offset, "next_offset": offset + 4096, "eof": eof})
		return string(data)
	}
	// Stale or duplicate pages are still stalls: a failed page must not buy time.
	stale := `{"ok":false,"buffer_id":"","error":"buffer 已失效或属于另一个 Agent"}`
	for turn := 1; turn <= auditProgressWarningTurns; turn++ {
		if g.creditPage("read_tool_buffer", stale) {
			t.Fatal("failed page credited")
		}
		if _, err := g.observe(tools.Snapshot{}); err != nil {
			t.Fatal(err)
		}
	}
	if g.turns != auditProgressWarningTurns {
		t.Fatalf("failed pages advanced window: %d", g.turns)
	}
	// Real pagination keeps the worker alive past the hard limit.
	for i := 0; i < auditProgressPageCredits; i++ {
		offset := int64(i * 4096)
		if !g.creditPage("read_tool_buffer", page(offset, true, false)) {
			t.Fatalf("genuine page %d not credited", i)
		}
		if g.creditPage("read_tool_buffer", page(offset, true, false)) {
			t.Fatal("same offset credited twice")
		}
		if _, err := g.observe(tools.Snapshot{}); err != nil {
			t.Fatalf("paging killed at page %d: %v", i, err)
		}
	}
	// The cap still forces a conclusion instead of allowing endless paging.
	if g.creditPage("read_tool_buffer", page(int64(auditProgressPageCredits*4096), true, false)) {
		t.Fatal("credit cap not enforced")
	}
	var err error
	for i := 0; i <= auditProgressLimitTurns && err == nil; i++ {
		_, err = g.observe(tools.Snapshot{})
	}
	if err == nil || !strings.Contains(err.Error(), "审计停滞") {
		t.Fatalf("unbounded paging escaped the guard: %v", err)
	}
}

func TestAuditProgressCreditsNovelEvidenceButStaysCapped(t *testing.T) {
	g := newAuditProgressGuard(tools.Snapshot{}, config.AgentConfig{})
	snapshot := tools.Snapshot{}
	for i := 0; i < auditProgressEvidenceCredits; i++ {
		if !g.creditEvidence() {
			t.Fatalf("credit %d refused below cap", i)
		}
		for turn := 0; turn < auditProgressLimitTurns-1; turn++ {
			if _, err := g.observe(snapshot); err != nil {
				t.Fatalf("credit %d stopped early: %v", i, err)
			}
		}
	}
	if g.creditEvidence() {
		t.Fatal("credit granted past the cap")
	}
	var err error
	for turn := 0; turn < auditProgressLimitTurns && err == nil; turn++ {
		_, err = g.observe(snapshot)
	}
	if err == nil || !strings.Contains(err.Error(), "审计停滞") {
		t.Fatalf("uncapped read sweep was not stopped: %v", err)
	}
}

func TestAuditProgressKeepsReadingDistinctFilesPastTheStallWindow(t *testing.T) {
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, _ []llm.Message) (llm.ToolResponse, error) {
		calls++
		if calls > auditProgressLimitTurns+2 {
			return toolReply("end_audit", map[string]any{"summary": "done"}), nil
		}
		return toolReply("search_content", map[string]any{"pattern": fmt.Sprintf("needle-%d", calls), "path": "."}), nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.cfg.Agent.InfiniteMode = true
	a.Run(context.Background(), "inspect", func(Event) {})
	if a.runErr != nil && strings.Contains(a.runErr.Error(), "审计停滞") {
		t.Fatalf("distinct new evidence was treated as spinning: calls=%d err=%v", calls, a.runErr)
	}
	if calls <= auditProgressLimitTurns {
		t.Fatalf("worker never got past the stall window: calls=%d", calls)
	}
}

func TestAuditProgressConvergenceGateBlocksEvidenceToolsOnly(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.cfg.Agent.InfiniteMode = true
	a.auditProgress = newAuditProgressGuard(tools.Snapshot{}, config.AgentConfig{})
	for i := 0; i < auditProgressWarningTurns; i++ {
		a.auditProgress.observe(tools.Snapshot{})
	}
	if !a.auditProgress.converging() {
		t.Fatal("guard did not reach the convergence window")
	}
	for _, tool := range []string{"read_file", "search_content", "list_files", "review_state", "read_handoff", "git_inspect"} {
		if reason := a.convergenceCorrection(tool); !strings.Contains(reason, "提交结论") {
			t.Fatalf("%s not blocked: %q", tool, reason)
		}
	}
	for _, tool := range []string{"file_review_update", "variable_review_update", "flow_review_update", "report_finding", "verify_finding", "todo_update"} {
		if reason := a.convergenceCorrection(tool); reason != "" {
			t.Fatalf("%s blocked: %q", tool, reason)
		}
	}
	// A fresh conclusion reopens the reading window.
	a.auditProgress.turns = 0
	if reason := a.convergenceCorrection("read_file"); reason != "" {
		t.Fatalf("read_file still blocked after conclusion: %q", reason)
	}
}
