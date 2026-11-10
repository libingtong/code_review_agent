package agent

import (
	"context"
	"strings"
	"testing"

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
	// An automatic moderator reactivation must not buy another 128 requests.
	a.Run(context.Background(), "continue", func(Event) {})
	if calls != auditProgressLimitTurns {
		t.Fatalf("reactivation reset guard: calls=%d", calls)
	}
}

func TestAuditProgressOnlyNewConclusionsResetWindow(t *testing.T) {
	s := tools.Snapshot{Files: []tools.FileReview{{Path: "a.go", Status: "reviewing"}}}
	g := newAuditProgressGuard(s)
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
