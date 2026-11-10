package agent

import (
	"code-review-agent/internal/llm"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditRunContinuesConcreteTaskInsteadOfNegotiatingAgain(t *testing.T) {
	stop := errors.New("captured first request")
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		for _, m := range messages {
			if m.Role != llm.RoleUser {
				continue
			}
			for _, stale := range []string{"先通过论坛协商", "请调用 read_handoff 按需读取完整地图", "你是全新审计团队", "系统没有预先分配", "先通过 forum_post"} {
				if strings.Contains(m.Content, stale) { t.Errorf("request restarts audit with %q", stale) }
			}
		}
		return llm.ToolResponse{}, stop
	}}
	team := newTestTeam(t, client)
	template, err := os.ReadFile(filepath.Join("..", "..", "prompts", "templates", "initial_audit_instruction.md"))
	if err != nil {
		t.Fatal(err)
	}
	team.prompts.Templates["initial_audit_instruction"] = string(template)
	team.auditScope = []string{"entry1.go"}
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	a.handoff = "saved handoff"
	a.Run(context.Background(), "inspect fixtures", func(Event) {})
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if !errors.Is(a.runErr, stop) {
		t.Fatalf("run err=%v", a.runErr)
	}
}
