package agent

import (
	"context"
	"strings"
	"testing"
)

func TestAuditStagePreAssignsScopeSlices(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.phase = phaseAudit
	team.auditScope = []string{"entry1.go", "entry2.go", "entry3.go", "entry4.go"}
	team.createStageLocked(phaseAudit)
	team.mu.Unlock()

	seen := map[string]int{}
	for _, w := range team.workers {
		if w.saved.Status.Phase != phaseAudit {
			continue
		}
		files := w.agent.tools.Snapshot().Files
		for _, file := range files {
			seen[file.Path]++
		}
		if len(files) > 0 && !strings.Contains(w.agent.assignment, "已经定") && !strings.Contains(w.agent.assignment, "切好分给你") {
			t.Fatalf("worker %s assignment lacks the slice: %q", w.agent.id, w.agent.assignment)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("scope split lost files: %v", seen)
	}
	for path, count := range seen {
		if count != 1 {
			t.Fatalf("file %s assigned %d times", path, count)
		}
	}
}

func TestAuditClaimGateBlocksForumUntilFirstClaim(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	worker := &Agent{cfg: team.cfg, phase: phaseAudit, turn: 5}
	if msg := worker.phaseToolCorrection("forum_roster"); !strings.Contains(msg, "认领") {
		t.Fatalf("forum browsing must be gated before the first claim: %q", msg)
	}
	if msg := worker.phaseToolCorrection("file_review_update"); msg != "" {
		t.Fatalf("claiming must stay allowed: %q", msg)
	}
	if msg := worker.phaseToolCorrection("review_state"); msg != "" {
		t.Fatalf("status read must stay allowed: %q", msg)
	}
	worker.fileReviewCalls = 1
	if msg := worker.phaseToolCorrection("forum_roster"); msg != "" {
		t.Fatalf("forum must reopen after a claim: %q", msg)
	}
}

func TestReadHandoffIsCapped(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.createStageLocked(phaseRecon)
	team.mu.Unlock()
	a := team.workers[0].agent
	a.handoff = `{"ok":true}`
	args := []byte(`{}`)
	for i := 0; i < maxHandoffReads; i++ {
		if out, _ := a.callTool(context.Background(), func(Event) {}, ToolCall{Name: "read_handoff", Arguments: args}); !strings.Contains(out, `"ok":true`) {
			t.Fatalf("handoff read %d failed: %s", i+1, out)
		}
	}
	out, _ := a.callTool(context.Background(), func(Event) {}, ToolCall{Name: "read_handoff", Arguments: args})
	if !strings.Contains(out, "交接已读过两遍") {
		t.Fatalf("handoff re-read must be capped: %s", out)
	}
}
