package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/llm"
)

func TestReconFirstOverBudgetRequestIncludesHandoffReminder(t *testing.T) {
	stop := errors.New("captured first over-budget request")
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		found := false
		for _, m := range messages {
			if m.Role == llm.RoleUser && strings.Contains(m.Content, "只能调用 audit_plan_done") {
				found = true
			}
		}
		if !found {
			t.Error("first request with only handoff tool lacks hard handoff reminder")
		}
		return llm.ToolResponse{}, stop
	}}
	team := newTestTeam(t, client)
	defer team.Close()
	team.cfg.Agent.MaxTurnsPerStage = 2
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "recon-1", phaseRecon, team.board)
	defer a.tools.Close()
	a.turn = 2
	a.Run(context.Background(), "continue recon", func(Event) {})
	if calls != 1 || !errors.Is(a.runErr, stop) {
		t.Fatalf("calls=%d err=%v", calls, a.runErr)
	}
}

func TestReconStaleToolAtBudgetCanCorrectToHandoff(t *testing.T) {
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		if calls == 1 {
			return toolReply("read_tool_buffer", map[string]any{"buffer_id": "stale"}), nil
		}
		paired := false
		for _, m := range messages {
			if m.Type == "function_call_output" && strings.Contains(m.Content, "侦察预算已用尽") {
				paired = true
			}
		}
		if !paired {
			t.Error("correction omitted paired output explaining the handoff-only restriction")
		}
		return toolReply("audit_plan_done", map[string]any{}), nil
	}}
	team := newTestTeam(t, client)
	defer team.Close()
	team.cfg.Agent.MaxTurnsPerStage = 2
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "recon-1", phaseRecon, team.board)
	defer a.tools.Close()
	a.turn = 3
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := a.chatStream(ctx, func(Event) {})
	if err != nil || calls != 2 || len(response.Calls) != 1 || response.Calls[0].Name != "audit_plan_done" {
		t.Fatalf("calls=%d response=%+v err=%v", calls, response, err)
	}
}
