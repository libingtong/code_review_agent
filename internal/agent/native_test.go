package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestNativeLoopRetriesProtocolFailuresUntilValidCall(t *testing.T) {
	calls := 0
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		if calls <= 5 {
			if calls == 1 {
				return llm.ToolResponse{Calls: []llm.FunctionCall{{Name: "forum_post", Arguments: `{"content":"unsafe"}`}}}, nil
			}
			found := false
			for _, message := range messages {
				if message.Role == llm.RoleUser && strings.Contains(message.Content, "立即纠正") && strings.Contains(message.Content, "第 ") {
					found = true
				}
			}
			if !found {
				t.Fatal("protocol retry omitted corrective feedback")
			}
			return llm.ToolResponse{Calls: []llm.FunctionCall{{Name: "forum_post", Arguments: `{"content":"unsafe"}`}}}, nil
		}
		response := toolReply("audit_plan_done", map[string]any{
			"summary":     "done",
			"audit_map":   "entry",
			"audit_files": []string{"entry1.go"},
		})
		response.ResponseID = "resp-tool-test"
		return response, nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.Run(context.Background(), "inspect", func(Event) {})
	if calls != 6 || a.runErr != nil || !a.completed || a.protocolFailures != 0 {
		t.Fatalf("protocol recovery calls=%d err=%v completed=%v streak=%d", calls, a.runErr, a.completed, a.protocolFailures)
	}
	if a.responseID != "resp-tool-test" || a.responseMessageIndex >= len(a.messages) || a.messages[a.responseMessageIndex].Type != "function_call_output" {
		t.Fatalf("continuation boundary=%q index=%d messages=%d", a.responseID, a.responseMessageIndex, len(a.messages))
	}
	for _, message := range a.messages {
		if message.Type == "function_call" && message.Name == "forum_post" {
			t.Fatal("invalid batch became native history")
		}
	}
	foundFeedback := false
	for _, message := range a.messages {
		if strings.Contains(message.Content, "第 5 次") {
			foundFeedback = true
		}
	}
	if !foundFeedback {
		t.Fatal("fifth corrective feedback was not retained")
	}
}

func TestNativeUnauthorizedCallPairsErrorAndValidCallResetsStreak(t *testing.T) {
	calls := 0
	client := &noticeClient{}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 2
	client.tools = func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		calls++
		switch calls {
		case 1:
			return toolReply("moderator_idle", map[string]any{}), nil
		case 2:
			if err := validateNativeHistory(messages); err != nil {
				t.Fatal(err)
			}
			paired := false
			for _, m := range messages {
				if m.Type == "function_call_output" && strings.Contains(m.Content, `"ok":false`) {
					paired = true
				}
			}
			if !paired {
				t.Fatal("unauthorized native call has no paired error")
			}
			return toolReply("read_file", map[string]any{"path": "missing-file.go"}), nil
		case 3, 4:
			return llm.ToolResponse{Thinking: "still selecting"}, nil
		default:
			return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 1}), nil
		}
	}
	a.Run(context.Background(), "inspect", func(Event) {})
	if calls != 5 || a.protocolFailures != 0 {
		t.Fatalf("valid business error did not reset streak: calls=%d err=%v streak=%d", calls, a.runErr, a.protocolFailures)
	}
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRoleDefinitionsMatchExecutionBoundary(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	for _, role := range []string{phaseRecon, phaseAudit, phaseModerator, "verifier"} {
		phase := role
		if role == "verifier" {
			phase = phaseAudit
		}
		a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), role, phase, team.board)
		defer a.tools.Close()
		a.verifying = role == "verifier"
		names := map[string]bool{}
		for _, d := range a.toolDefinitions() {
			names[d.Name] = true
			if !json.Valid(d.Parameters) {
				t.Fatalf("invalid schema %s", d.Name)
			}
		}
		if !names["read_file"] || !names["read_handoff"] {
			t.Fatalf("%s cannot inspect evidence", role)
		}
		if names["moderator_idle"] != (role == phaseModerator) || names["forum_announce"] != (role == phaseModerator) || names["forum_moderate"] != (role == phaseModerator) {
			t.Fatalf("moderator privilege leaked to %s", role)
		}
		if names["audit_plan_done"] != (role == phaseRecon) || names["report_finding"] != (role == phaseAudit) || names["end_audit"] != (role == phaseAudit) || names["verify_finding"] != (role == phaseAudit) {
			t.Fatalf("role boundary leaked: %s %v", role, names)
		}
	}
}

func TestNativeResultsTraceRestoreAndCompressionPreserveEvidence(t *testing.T) {
	client := &noticeClient{tools: func(context.Context, []llm.Message) (llm.ToolResponse, error) {
		return toolReply("read_file", map[string]any{"path": "entry1.go", "limit": 10}), nil
	}}
	team := newTestTeam(t, client)
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent
	a.cfg.Agent.MaxTurns = 1
	a.cfg.Agent.LogSession = true
	a.cfg.Agent.LogSessionDir = t.TempDir()
	a.Run(context.Background(), "inspect", func(Event) {})
	if err := validateNativeHistory(a.messages); err != nil {
		t.Fatal(err)
	}
	var native []llm.Message
	for _, m := range a.messages {
		if m.Type != "" {
			native = append(native, m)
		}
	}
	if len(native) != 2 || native[0].CallID != native[1].CallID || !strings.Contains(native[1].Content, "buffer_id") {
		t.Fatal("bounded native result lost pairing/buffer")
	}
	raw, err := os.ReadFile(a.TracePath())
	if err != nil {
		t.Fatal(err)
	}
	var trace []llm.Message
	if err = json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	full := false
	for _, m := range trace {
		if m.Type == "function_call_output" && m.CallID == native[0].CallID && strings.Contains(m.Content, strings.Repeat("中文证据", 1000)) {
			full = true
		}
	}
	if !full {
		t.Fatal("trace omitted full result or native call id")
	}
	session := filepath.Join(t.TempDir(), "native.json")
	if err = team.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, client)
	if err = restored.LoadSession(session); err != nil {
		t.Fatal(err)
	}
	b := restored.workers[0].agent
	var restoredNative []llm.Message
	for _, m := range b.messages {
		if m.Type != "" {
			restoredNative = append(restoredNative, m)
		}
	}
	if !reflect.DeepEqual(native, restoredNative) {
		t.Fatal("restore altered exact native records")
	}
	b.phase = phaseModerator
	b.prompts.Templates["compress_user"] = "!{state_and_conversation}"
	summaryCalls := 0
	b.compressClient = &noticeClient{call: func(_ context.Context, messages []llm.Message) (string, error) {
		summaryCalls++
		for _, m := range messages {
			if m.Type != "" {
				t.Fatal("summarizer received executable tool items")
			}
		}
		if !strings.Contains(messages[1].Content, native[0].CallID) || !strings.Contains(messages[1].Content, "read_file") {
			t.Fatal("summarizer lost native evidence")
		}
		return "read_file evidence retained", nil
	}}
	if err = b.compressContext(context.Background(), func(Event) {}, "native history"); err != nil {
		t.Fatal(err)
	}
	if summaryCalls != 1 || !strings.Contains(b.messages[0].Content, "论坛管理员") {
		t.Fatal("moderator compression lost role")
	}
	for _, m := range b.messages {
		if m.Type != "" {
			t.Fatal("compaction retained orphan native records")
		}
	}
}

// Providers may return several parallel calls in one turn. The batch is no
// longer fatal: recordResponse stores every call once, and unexecuted calls get
// only an output — duplicating the call item would poison history.
func TestMultipleNativeCallsAreAcceptedWithSkips(t *testing.T) {
	response := llm.ToolResponse{Calls: []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "c2", Name: "search_content", Arguments: `{"query":"admin"}`},
		{CallID: "c3", Name: "forum_roster", Arguments: `{}`},
	}}
	if err := validateNativeResponse(response, nil); err != nil {
		t.Fatalf("multi-call batch must be accepted: %v", err)
	}

	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-9", phaseAudit, team.board)
	defer a.tools.Close()
	a.recordResponse(response)
	// The first call is the one the turn would execute; the rest are acknowledged.
	a.rejectTool(response.Calls[0], "已执行")
	a.recordSkippedOutputs(response.Calls[1:], "本回合未执行")

	callItems, outputItems := 0, 0
	seen := map[string]int{}
	for _, message := range a.messages {
		switch message.Type {
		case "function_call":
			callItems++
			seen[message.CallID]++
		case "function_call_output":
			outputItems++
		}
	}
	if callItems != 3 || outputItems != 3 {
		t.Fatalf("history shape wrong: calls=%d outputs=%d", callItems, outputItems)
	}
	for id, count := range seen {
		if count > 1 {
			t.Fatalf("call_id %q recorded %d times (history would be rejected)", id, count)
		}
	}
	// The exact validation that used to fail: replays must accept this history.
	if err := llm.ValidateToolHistory(a.messages); err != nil {
		t.Fatalf("history rejected by provider validation: %v", err)
	}
}

// A malformed call inside a multi-call batch still fails the turn.
func TestInvalidCallInsideBatchIsRejected(t *testing.T) {
	response := llm.ToolResponse{Calls: []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "", Name: "search_content", Arguments: `{}`},
	}}
	if err := validateNativeResponse(response, nil); err == nil {
		t.Fatal("missing call_id must be rejected")
	}
}

// A read-only batch runs in the model's order, duplicates collapse, and the
// per-turn cap is respected.
func TestPlanBatchOrdersDedupesAndCaps(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-8", phaseAudit, team.board)
	defer a.tools.Close()
	a.cfg.Agent.MaxToolsPerTurn = 3

	calls := []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "c2", Name: "search_content", Arguments: `{"query":"admin"}`},
		{CallID: "c3", Name: "read_file", Arguments: `{"path":"a.go"}`}, // duplicate of c1
		{CallID: "c4", Name: "list_files", Arguments: `{}`},             // over the cap
	}
	run, skipped := a.planBatch(calls)
	if len(run) != 3 || len(skipped) != 1 {
		t.Fatalf("run=%d skipped=%d", len(run), len(skipped))
	}
	if run[0].CallID != "c1" || run[1].CallID != "c2" || run[2].CallID != "c4" {
		t.Fatalf("order/cap wrong: %+v", run)
	}
	if skipped[0].CallID != "c3" {
		t.Fatalf("duplicate should be the skipped one: %+v", skipped)
	}
}

// An exclusive tool takes the whole turn; everything else is acknowledged.
func TestPlanBatchGivesExclusiveToolTheTurn(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-7", phaseAudit, team.board)
	defer a.tools.Close()
	a.cfg.Agent.MaxToolsPerTurn = 3

	calls := []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "c2", Name: "read_tool_buffer", Arguments: `{"buffer_id":"b1","offset":0}`},
		{CallID: "c3", Name: "search_content", Arguments: `{"query":"x"}`},
	}
	run, skipped := a.planBatch(calls)
	if len(run) != 1 || run[0].Name != "read_tool_buffer" {
		t.Fatalf("exclusive call must run alone: %+v", run)
	}
	if len(skipped) != 2 {
		t.Fatalf("others must be acknowledged: %+v", skipped)
	}
}

// Only one state-writing call per turn is applied.
func TestPlanBatchAllowsSingleStateWrite(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-6", phaseAudit, team.board)
	defer a.tools.Close()
	a.cfg.Agent.MaxToolsPerTurn = 3

	calls := []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "c2", Name: "file_review_update", Arguments: `{"path":"a.go","status":"reviewed"}`},
		{CallID: "c3", Name: "todo_create", Arguments: `{"title":"x"}`},
	}
	run, skipped := a.planBatch(calls)
	if len(run) != 2 {
		t.Fatalf("run = %+v", run)
	}
	if run[0].Name != "read_file" || run[1].Name != "file_review_update" {
		t.Fatalf("first write should win: %+v", run)
	}
	if len(skipped) != 1 || skipped[0].Name != "todo_create" {
		t.Fatalf("second write must be skipped: %+v", skipped)
	}
}

// recordResponse must keep one call item per call_id so the provider sees a
// complete assistant turn.
func TestRecordResponseKeepsEveryCall(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-5", phaseAudit, team.board)
	defer a.tools.Close()

	a.recordResponse(llm.ToolResponse{
		Content: "先看这三个文件",
		Calls: []llm.FunctionCall{
			{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
			{CallID: "c2", Name: "read_file", Arguments: `{"path":"b.go"}`},
		},
	})
	calls, assistant := 0, 0
	for _, message := range a.messages {
		switch message.Type {
		case "function_call":
			calls++
		case "":
			if message.Role == llm.RoleAssistant {
				assistant++
			}
		}
	}
	if calls != 2 || assistant != 1 {
		t.Fatalf("history shape wrong: calls=%d assistant=%d", calls, assistant)
	}
}
