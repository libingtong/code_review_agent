package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestNativeCompatAliasStrictRequestScope(t *testing.T) {
	definitions := []llm.ToolDefinition{{Name: "read_file"}, {Name: "file_review_update"}}
	for _, tc := range []struct{ input, want string }{
		{"CompatReadFile42697e", "read_file"}, {"CompatFileReviewUpdate95439b", "file_review_update"},
		{"CompatReadFileABC123", "read_file"}, {"read_file", "read_file"},
		{"CompatReadFile42697", "CompatReadFile42697"}, {"CompatReadFile42697ef", "CompatReadFile42697ef"},
		{"CompatReadFile42697g", "CompatReadFile42697g"}, {"CompatreadFile42697e", "CompatreadFile42697e"},
		{"ReadFile42697e", "ReadFile42697e"}, {"CompatModeratorIdle123abc", "CompatModeratorIdle123abc"},
		{"CompatReadFile42697e.extra", "CompatReadFile42697e.extra"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			before := llm.FunctionCall{ID: "fc-1", CallID: "call-1", Name: tc.input, Arguments: `{"path":"a.go"}`}
			response := llm.ToolResponse{Calls: []llm.FunctionCall{before}, Content: "kept", ResponseID: "resp-1"}
			normalizeNativeToolAliases(&response, definitions)
			before.Name = tc.want
			if !reflect.DeepEqual(response.Calls, []llm.FunctionCall{before}) || response.Content != "kept" || response.ResponseID != "resp-1" {
				t.Fatalf("unexpected normalization: %+v", response)
			}
		})
	}
	for _, defs := range [][]llm.ToolDefinition{nil, {{Name: "read_file"}, {Name: "read__file"}}} {
		response := llm.ToolResponse{Calls: []llm.FunctionCall{{Name: "CompatReadFile42697e"}}}
		normalizeNativeToolAliases(&response, defs)
		if response.Calls[0].Name != "CompatReadFile42697e" {
			t.Fatal("empty or ambiguous declarations admitted alias")
		}
	}
}

func TestNativeCompatAliasBeforePermissionGate(t *testing.T) {
	client := &noticeClient{tools: func(context.Context, []llm.Message) (llm.ToolResponse, error) {
		return llm.ToolResponse{Calls: []llm.FunctionCall{{ID: "fc-1", CallID: "call-1", Name: "CompatReadFile42697e", Arguments: `{"path":"entry1.go"}`}}}, nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	response, err := a.chatStream(context.Background(), func(Event) {})
	if err != nil || len(response.Calls) != 1 || response.Calls[0].Name != "read_file" || response.Calls[0].ID != "fc-1" || a.protocolFailures != 0 {
		t.Fatalf("response=%+v err=%v failures=%d", response, err, a.protocolFailures)
	}
}

func TestNativeUnknownBatchPairsEveryCallAndDiagnosesProtocol(t *testing.T) {
	for _, unknownFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "unknown-first", false: "unknown-second"}[unknownFirst], func(t *testing.T) {
			requests := 0
			client := &noticeClient{}
			team := newTestTeam(t, client)
			team.createStageLocked(phaseRecon)
			a := team.workers[0].agent
			client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
				requests++
				if requests == 1 {
					calls := []llm.FunctionCall{{CallID: "bad", Name: "CompatModeratorIdle123abc", Arguments: `{}`}, {CallID: "valid", Name: "read_file", Arguments: `{"path":"entry1.go"}`}}
					if !unknownFirst {
						calls[0], calls[1] = calls[1], calls[0]
					}
					return llm.ToolResponse{Calls: calls}, nil
				}
				if err := validateNativeHistory(messages); err != nil {
					t.Fatal(err)
				}
				outputs := 0
				feedback := false
				for _, m := range messages {
					if m.Type == "function_call_output" && (m.CallID == "bad" || m.CallID == "valid") {
						outputs++
						if strings.Contains(m.Content, "历史修复") {
							t.Fatal("batch deferred to history repair")
						}
					}
					if m.Role == llm.RoleUser && strings.Contains(m.Content, "原生工具协议失败") && strings.Contains(m.Content, "CompatModeratorIdle123abc") {
						feedback = true
					}
				}
				if outputs != 2 || !feedback {
					t.Fatalf("outputs=%d feedback=%v", outputs, feedback)
				}
				return llm.ToolResponse{Calls: []llm.FunctionCall{{CallID: "recovery", Name: "read_file", Arguments: `{"path":"entry1.go"}`}}}, nil
			}
			_, err := a.chatStream(context.Background(), func(Event) {})
			if err != nil || requests != 2 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestNativeUnknownCallStopKeepsProtocolCause(t *testing.T) {
	client := &noticeClient{tools: func(context.Context, []llm.Message) (llm.ToolResponse, error) {
		return llm.ToolResponse{Calls: []llm.FunctionCall{{CallID: "bad", Name: "CompatModeratorIdle123abc", Arguments: `{}`}}}, nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxProtocolFailures = 1
	_, err := a.chatStream(context.Background(), func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "原生工具协议失败") || !strings.Contains(err.Error(), "CompatModeratorIdle123abc") || strings.Contains(err.Error(), "没有返回") {
		t.Fatalf("wrong diagnosis: %v", err)
	}
}
