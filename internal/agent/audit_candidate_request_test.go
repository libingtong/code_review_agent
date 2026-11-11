package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func TestConfirmedCandidateTakesPriorityInActualRequest(t *testing.T) {
	stop := errors.New("captured candidate request")
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		var last string
		for _, m := range messages {
			if strings.HasPrefix(m.Content, assignmentDirective) {
				last = m.Content
			}
		}
		if !strings.Contains(last, "candidate-alpha") || !strings.Contains(last, "report_finding") {
			t.Errorf("candidate not prioritized: %s", last)
		}
		if strings.Contains(last, "优先完成第一个文件") {
			t.Errorf("file checklist overrides candidate: %s", last)
		}
		return llm.ToolResponse{}, stop
	}}
	team := newTestTeam(t, client)
	team.auditScope = []string{"entry1.go"}
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	snapshot := a.tools.Snapshot()
	snapshot.Flows = []tools.FlowReview{{Name: "candidate-alpha", Status: "confirmed", Evidence: "entry -> missing authorization -> sink"}}
	a.tools.RestoreSnapshot(snapshot)
	_, err := a.chatStreamOnce(context.Background(), func(Event) {})
	if !errors.Is(err, stop) {
		t.Fatalf("err=%v", err)
	}
	if len(a.tools.Snapshot().Findings) != 0 {
		t.Fatal("priority guidance manufactured a finding")
	}
}

func TestVerificationRemovesMainAuditChecklist(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.auditScope = []string{"entry1.go"}
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.reinforceAssignment()
	a.verifying = true
	a.reinforceAssignment()
	for _, m := range a.messages {
		if strings.HasPrefix(m.Content, assignmentDirective) {
			t.Fatalf("verification retains checklist: %s", m.Content)
		}
	}
}
