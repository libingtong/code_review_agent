package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
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
		w.agent.tools.Call("file_review_update", json.RawMessage(`{"pattern":"*","status":"reviewing"}`))
		if after := w.agent.tools.Snapshot().Files; len(after) != len(files) {
			t.Fatalf("audit worker %s selector escaped its slice: before=%d after=%d", w.agent.id, len(files), len(after))
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
	for _, name := range []string{"forum_roster", "forum_read", "forum_post", "read_handoff", "review_state", "load_skill"} {
		if msg := worker.phaseToolCorrection(name); !strings.Contains(msg, "认领") {
			t.Fatalf("%s must be gated before the first claim: %q", name, msg)
		}
	}
	if msg := worker.phaseToolCorrection("file_review_update"); msg != "" {
		t.Fatalf("claiming must stay allowed: %q", msg)
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
	a.fileReviewCalls = 1
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

	// Reading leaves an audit in progress; only a conclusion clears the queue.
	a.tools.PlanAuditScope(a.assigned)
	for _, path := range a.assigned {
		a.tools.Call("read_file", json.RawMessage(`{"path":"`+path+`","limit":5}`))
	}
	a.reinforceAssignment()
	if got := countAssignmentDirectives(a.messages); got != 1 {
		t.Fatalf("read files still need conclusions: directive count = %d", got)
	}
	snapshot := a.tools.Snapshot()
	for i := range snapshot.Files {
		snapshot.Files[i].Status = "reviewed"
	}
	a.tools.RestoreSnapshot(snapshot)
	a.reinforceAssignment()
	if got := countAssignmentDirectives(a.messages); got != 0 {
		t.Fatalf("checklist must clear when everything is completed: %d", got)
	}
}

func TestReinforceAssignmentAdvancesOnlyAfterConclusion(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.createStageLocked(phaseRecon)
	team.mu.Unlock()
	a := team.workers[0].agent
	a.phase = phaseAudit
	a.assigned = []string{"first.go", "second.go", "third.go"}
	a.tools.RestoreSnapshot(tools.Snapshot{Files: []tools.FileReview{
		{Path: "first.go", Status: "unseen"},
		{Path: "second.go", Status: "reviewing"},
		{Path: "third.go", Status: "skipped"},
	}})
	a.reinforceAssignment()
	text := lastAssignmentDirective(a.messages)
	if !strings.Contains(text, "2 个未完成") || !strings.Contains(text, "first.go、second.go") || strings.Contains(text, "third.go") {
		t.Fatalf("unseen/reviewing must stay queued, skipped must leave: %q", text)
	}
	if !strings.Contains(text, "已读则") || !strings.Contains(text, "file_review_update") || strings.Contains(text, "个没读") {
		t.Fatalf("instruction must distinguish reading from an audit conclusion: %q", text)
	}
	snapshot := a.tools.Snapshot()
	snapshot.Files[0].Status = "reviewed"
	a.tools.RestoreSnapshot(snapshot)
	for i := 0; i < 5; i++ {
		a.reinforceAssignment()
	}
	text = lastAssignmentDirective(a.messages)
	if strings.Contains(text, "first.go") || !strings.Contains(text, "1 个未完成") || !strings.Contains(text, "second.go") {
		t.Fatalf("queue did not advance after conclusion: %q", text)
	}
	if countAssignmentDirectives(a.messages) != 1 {
		t.Fatalf("checklist accumulated across repeated requests: %v", a.messages)
	}
	// Reminders must never manufacture review completion or alter coverage.
	after := a.tools.Snapshot()
	if after.Files[1].Status != "reviewing" {
		t.Fatalf("reminder changed unfinished file status: %+v", after.Files[1])
	}
}

func TestReinforceAssignmentBoundsPreview(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.createStageLocked(phaseRecon)
	team.mu.Unlock()
	a := team.workers[0].agent
	a.phase = phaseAudit
	for i := 0; i < 100; i++ {
		a.assigned = append(a.assigned, strings.Repeat("长路径", 200)+".go")
	}
	a.reinforceAssignment()
	text := lastAssignmentDirective(a.messages)
	if !strings.Contains(text, "100 个未完成") || len(text) > 1600 {
		t.Fatalf("preview must be bounded independently of queue/path lengths: %d bytes", len(text))
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
