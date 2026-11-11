package agent

import (
	"context"
	"encoding/json"
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
