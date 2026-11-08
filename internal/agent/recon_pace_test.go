package agent

import (
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/tools"
)

func paceTestWorker(t *testing.T, limit int) *Agent {
	t.Helper()
	cfg := config.Config{}
	cfg.Agent.MaxTurnsPerStage = limit
	cfg.Agent.MinAuditFiles = 8
	registry, err := tools.NewRegistry(t.TempDir(), 1024)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return &Agent{cfg: cfg, phase: phaseRecon, tools: registry}
}

// A recon worker that never submits must be told to stop browsing the forum at
// half budget, not at the very end: in the 15:00 run four workers burned 25
// minutes on forum_read/list_files and reached turn 178 with an empty map.
func TestReconPacingLadder(t *testing.T) {
	a := paceTestWorker(t, 90)

	a.turn = 10
	if stage, text := a.reconPacing(); stage != 0 || text != "" {
		t.Fatalf("early turn must stay silent, got stage=%d text=%q", stage, text)
	}

	a.turn = 60
	stage, text := a.reconPacing()
	if stage != 1 {
		t.Fatalf("half budget should start the soft nudge, got stage=%d", stage)
	}
	if !strings.Contains(text, "90") || !strings.Contains(text, "audit_plan_done") {
		t.Fatalf("soft nudge must mention the budget and the submit tool: %q", text)
	}

	a.turn = 95
	stage, text = a.reconPacing()
	if stage != 2 {
		t.Fatalf("spent budget must escalate, got stage=%d", stage)
	}
	if !strings.Contains(text, "只能调用 audit_plan_done") {
		t.Fatalf("hard nudge must restrict to submission: %q", text)
	}
}

func TestReconPacingKeepsForumOpenUntilHalfBudget(t *testing.T) {
	a := paceTestWorker(t, 90)

	a.turn = 20
	if msg := a.phaseToolCorrection("forum_read"); msg != "" {
		t.Fatalf("forum must stay open early: %q", msg)
	}
	if msg := a.phaseToolCorrection("audit_plan_done"); msg != "" {
		t.Fatalf("submit must always be allowed: %q", msg)
	}

	a.turn = 60
	for _, name := range []string{"forum_read", "forum_threads", "forum_roster"} {
		msg := a.phaseToolCorrection(name)
		if !strings.Contains(msg, "过半") {
			t.Fatalf("%s must be redirected after half budget, got %q", name, msg)
		}
	}
	if msg := a.phaseToolCorrection("read_file"); msg != "" {
		t.Fatalf("reading source must stay allowed: %q", msg)
	}
	if msg := a.phaseToolCorrection("forum_post"); msg != "" {
		t.Fatalf("posting progress must stay allowed: %q", msg)
	}

	a.turn = 95
	for _, name := range []string{"read_file", "forum_read", "read_tool_buffer"} {
		if msg := a.phaseToolCorrection(name); !strings.Contains(msg, "只能调用 audit_plan_done") {
			t.Fatalf("%s must be blocked once the budget is spent, got %q", name, msg)
		}
	}
	if msg := a.phaseToolCorrection("audit_plan_done"); msg != "" {
		t.Fatalf("submit must stay allowed at the end: %q", msg)
	}
}

func TestReconPacingCountsMappedFiles(t *testing.T) {
	a := paceTestWorker(t, 90)
	a.tools.RestoreSnapshot(tools.Snapshot{Files: []tools.FileReview{
		{Path: "src/a.go", Status: "reviewing"},
		{Path: "src/b.go", Status: "reviewing"},
		{Path: "", Status: "reviewing"},
	}})
	a.turn = 60
	if _, text := a.reconPacing(); !strings.Contains(text, "2 个文件") {
		t.Fatalf("nudge must report how many files are mapped: %q", text)
	}
}
