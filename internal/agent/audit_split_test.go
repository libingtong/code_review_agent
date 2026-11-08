package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
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
	if !strings.Contains(out, "以上为节选") {
		t.Fatalf("handoff re-read must return a bounded excerpt: %s", out)
	}
}

// 93 load_skill and 150 review_state calls in 23 minutes while only 3 findings were
// filed: both meta tools need a bound.
func TestMetaToolsAreBounded(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.createStageLocked(phaseRecon)
	team.mu.Unlock()
	a := team.workers[0].agent
	a.phase = phaseAudit
	a.turn = 10
	a.skillLoads = maxSkillLoads
	if msg := a.phaseToolCorrection("load_skill"); !strings.Contains(msg, "浪费回合") {
		t.Fatalf("skill loading must be capped: %q", msg)
	}

	// The first review_state returns the board; an unchanged second call is answered
	// with a one-liner.
	first, _ := a.callTool(context.Background(), func(Event) {}, ToolCall{Name: "review_state", Arguments: []byte(`{}`)})
	if !strings.Contains(first, "files") && !strings.Contains(first, "文件") {
		t.Fatalf("first review_state must return the board: %s", first[:80])
	}
	second, _ := a.callTool(context.Background(), func(Event) {}, ToolCall{Name: "review_state", Arguments: []byte(`{}`)})
	if !strings.Contains(second, "完全一致") {
		t.Fatalf("unchanged review_state must be short-circuited: %s", second)
	}
}

// A 35-minute stage ended with audit-3's assigned HybridEncryption.java still
// "pending": the assignment lived only in the system prompt. The checklist now rides
// along with every request and is replaced, never stacked.
func TestReinforceAssignmentKeepsChecklistInFront(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.createStageLocked(phaseRecon)
	team.mu.Unlock()
	a := team.workers[0].agent
	a.phase = phaseAudit
	a.assigned = []string{"entry1.go", "entry2.go"}

	a.reinforceAssignment()
	if got := countAssignmentDirectives(a.messages); got != 1 {
		t.Fatalf("directive count = %d, want 1", got)
	}
	if !strings.Contains(lastAssignmentDirective(a.messages), "entry1.go") {
		t.Fatalf("directive must name the unread file: %q", lastAssignmentDirective(a.messages))
	}

	a.reinforceAssignment()
	if got := countAssignmentDirectives(a.messages); got != 1 {
		t.Fatalf("directive stacked instead of replaced: %d", got)
	}

	// Once both files are read, the checklist disappears.
	a.tools.PlanAuditScope(a.assigned)
	for _, path := range a.assigned {
		a.tools.Call("read_file", json.RawMessage(`{"path":"`+path+`","limit":5}`))
	}
	a.reinforceAssignment()
	if got := countAssignmentDirectives(a.messages); got != 0 {
		t.Fatalf("checklist must clear when everything is read: %d", got)
	}
}

func countAssignmentDirectives(messages []llm.Message) int {
	count := 0
	for _, message := range messages {
		if message.Type == "" && strings.HasPrefix(message.Content, assignmentDirective) {
			count++
		}
	}
	return count
}

func lastAssignmentDirective(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type == "" && strings.HasPrefix(messages[i].Content, assignmentDirective) {
			return messages[i].Content
		}
	}
	return ""
}
