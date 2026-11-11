package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
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
				if message.Role == llm.RoleUser && strings.Contains(message.Content, "原生工具协议失败") && strings.Contains(message.Content, "第 ") {
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
	team.cfg.Agent.MaxProtocolFailures = 10 // fixture recovers after several failures
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

// A model that keeps answering in prose must not loop forever: the member is
// stopped after max_protocol_failures and the prose is never replayed.
func TestProtocolFailureCapStopsMember(t *testing.T) {
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		for _, message := range messages {
			if strings.Contains(message.Content, "WRITTEN_AS_PROSE") {
				t.Fatalf("failed prose answer must not be replayed into history")
			}
		}
		return llm.ToolResponse{Content: "WRITTEN_AS_PROSE 我要调用 forum_post"}, nil
	}}
	team := newTestTeam(t, client)
	defer team.Close()
	team.cfg.Agent.MaxProtocolFailures = 2
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-4", phaseAudit, team.board)
	defer a.tools.Close()
	_, err := a.chatStream(context.Background(), func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "连续 2 次没有返回原生工具调用") {
		t.Fatalf("loop must stop with a clear error, got %v", err)
	}
}

// An interrupted turn must not leave a call without an output: the next request
// would be rejected with "native history has unresolved function calls".
func TestRepairToolHistoryClosesUnfinishedCalls(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-3", phaseAudit, team.board)
	defer a.tools.Close()

	a.messages = []llm.Message{
		{Role: llm.RoleUser, Content: "看文件"},
		{Type: "function_call", CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{Type: "function_call", CallID: "c2", Name: "read_file", Arguments: `{"path":"b.go"}`},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
	}
	if err := llm.ValidateToolHistory(a.messages); err == nil {
		t.Fatal("fixture should start broken")
	}
	a.repairToolHistory()
	if err := llm.ValidateToolHistory(a.messages); err != nil {
		t.Fatalf("repair did not fix history: %v", err)
	}
	answers := 0
	for _, message := range a.messages {
		if message.Type == "function_call_output" && message.CallID == "c2" {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("unfinished call not closed exactly once: %d", answers)
	}
}

// A batch that is aborted mid-way (runErr) must still close every call it
// recorded, otherwise the next request is rejected.
func TestAbortedBatchClosesEveryCall(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-2", phaseAudit, team.board)
	defer a.tools.Close()

	response := llm.ToolResponse{Calls: []llm.FunctionCall{
		{CallID: "c1", Name: "read_file", Arguments: `{"path":"a.go"}`},
		{CallID: "c2", Name: "read_file", Arguments: `{"path":"b.go"}`},
	}}
	a.recordResponse(response)
	// First call executed, then the turn aborts before the second one runs.
	a.rejectTool(response.Calls[0], "已执行")
	a.runErr = context.Canceled
	a.recordSkippedOutputs(uniqueCalls(response.Calls[1:]), "回合被中断")
	if err := llm.ValidateToolHistory(a.messages); err != nil {
		t.Fatalf("aborted batch left broken history: %v", err)
	}
}

// The forum nudge fires once per agent and only when nothing was posted yet.
func TestForumNudgeInjectedOnce(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	countNudges := func(a *Agent) int {
		n := 0
		for _, message := range a.messages {
			if strings.Contains(message.Content, "你还没有在论坛发过言") {
				n++
			}
		}
		return n
	}

	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "recon-1", phaseRecon, team.board)
	defer a.tools.Close()
	a.turn = 3
	a.nudgeForumPost()
	if got := countNudges(a); got != 1 {
		t.Fatalf("nudge count = %d", got)
	}
	a.nudgeForumPost()
	if got := countNudges(a); got != 1 {
		t.Fatalf("nudge repeated: %d", got)
	}

	// An agent that already posted is never nudged.
	other := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "recon-2", phaseRecon, team.board)
	defer other.tools.Close()
	if _, err := team.board.Post("recon-2", phaseRecon, "*", 0, "范围", "我负责入口与鉴权"); err != nil {
		t.Fatal(err)
	}
	other.turn = 3
	other.nudgeForumPost()
	if got := countNudges(other); got != 0 {
		t.Fatalf("posted agent nudged %d times", got)
	}
}

// A continuation suffix that only carries tool results leaves the request with no
// text; MiniMax answers 400 "chat content is empty". Such a turn must send the
// full history instead.
func TestContinuationExtendsSuffixToText(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-1", phaseAudit, team.board)
	defer a.tools.Close()

	a.messages = []llm.Message{
		{Role: llm.RoleSystem, Content: "系统"},
		{Role: llm.RoleUser, Content: "开始"},
		{Type: "function_call", CallID: "c1", Name: "read_file", Arguments: `{}`},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
	}
	a.responseID = "resp_1"
	a.responseMessageIndex = 3 // suffix = only the tool output

	ctx := a.responsesRequestContext(context.Background())
	continuation, ok := llm.ResponsesContinuationFrom(ctx)
	if !ok {
		t.Fatal("continuation should be kept by pulling a text item into the suffix")
	}
	if len(continuation.Messages) != 3 || continuation.Messages[0].Content != "开始" {
		t.Fatalf("suffix should start at the nearest text message: %+v", continuation.Messages)
	}

	// With a text item in the suffix the optimisation stays enabled.
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "继续"})
	a.responseMessageIndex = 3
	ctx = a.responsesRequestContext(context.Background())
	if _, ok := llm.ResponsesContinuationFrom(ctx); !ok {
		t.Fatal("continuation should be kept when the suffix has text")
	}
}

// Compression can cut a tool pair; the replay must never carry orphan outputs.
func TestSanitizeDropsOrphanToolOutputs(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-1", phaseAudit, team.board)
	defer a.tools.Close()

	a.messages = []llm.Message{
		{Type: "function_call_output", CallID: "orphan", Content: `{"ok":true}`},
		{Role: llm.RoleUser, Content: "继续"},
		{Type: "function_call", CallID: "c1", Name: "read_file", Arguments: `{}`},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
	}
	a.sanitizeMessages()
	outputs := 0
	for _, message := range a.messages {
		if message.Type == "function_call_output" {
			outputs++
			if message.CallID == "orphan" {
				t.Fatal("orphan output survived sanitize")
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("outputs = %d", outputs)
	}
}

// Recon must be bounded: after the limit a worker is told to submit, then stopped.
func TestReconTurnLimitStopsRunawayWorker(t *testing.T) {
	client := &noticeClient{
		call: func(context.Context, []llm.Message) (string, error) { return "压缩摘要", nil },
		tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
			return toolReply("review_state", map[string]any{}), nil
		},
	}
	team := newTestTeam(t, client)
	defer team.Close()
	team.cfg.Agent.MaxTurnsPerStage = 2
	team.cfg.Agent.MaxTurns = 0
	team.cfg.Agent.InfiniteMode = true
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "recon-1", phaseRecon, team.board)
	defer a.tools.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.Run(ctx, "侦察这个仓库", func(Event) {})

	if a.runErr == nil || (!strings.Contains(a.runErr.Error(), "侦察回合超时") && !strings.Contains(a.runErr.Error(), "侦察预算已用尽")) {
		t.Fatalf("run should stop with a recon-budget error, got %v", a.runErr)
	}
	nudges := 0
	for _, message := range a.messages {
		// Count pacing reminders, not paired tool-error outputs or protocol
		// corrections, which also describe the exhausted recon budget.
		if message.Role == llm.RoleUser && strings.HasPrefix(message.Content, "侦察预算已用尽（") {
			nudges++
		}
	}
	if nudges < 1 || nudges > 2 {
		t.Fatalf("submit reminder count = %d, want 1-2", nudges)
	}
}

// A text message between a call and its output is rejected by providers
// ("interrupts an unresolved function call"); normalization must repair it.
func TestNormalizeToolHistoryMovesInterleavedText(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-1", phaseAudit, team.board)
	defer a.tools.Close()

	a.messages = []llm.Message{
		{Role: llm.RoleUser, Content: "开始"},
		{Type: "function_call", CallID: "c1", Name: "read_file", Arguments: `{}`},
		{Role: llm.RoleUser, Content: "插进来的通知"},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
	}
	a.normalizeToolHistory()
	if err := llm.ValidateToolHistory(a.messages); err != nil {
		t.Fatalf("normalized history still invalid: %v", err)
	}
	// The call is immediately followed by an output; the text moved after it.
	if a.messages[2].Type != "function_call_output" {
		t.Fatalf("call not followed by output: %+v", a.messages)
	}
	last := a.messages[len(a.messages)-1]
	if last.Type != "" || last.Content != "插进来的通知" {
		t.Fatalf("interleaved text lost or misplaced: %+v", a.messages)
	}
}

// A prose-only moderator turn becomes moderator_idle instead of a protocol
// failure: the moderator supervises, it must not die on politeness.
func TestModeratorProseBecomesIdle(t *testing.T) {
	client := &noticeClient{tools: func(_ context.Context, messages []llm.Message) (llm.ToolResponse, error) {
		return llm.ToolResponse{Content: "一切正常，无需干预。"}, nil
	}}
	team := newTestTeam(t, client)
	defer team.Close()
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), phaseModerator, phaseModerator, team.board)
	defer a.tools.Close()

	response, err := a.chatStream(context.Background(), func(Event) {})
	if err != nil {
		t.Fatalf("prose-only moderator turn must not fail: %v", err)
	}
	if len(response.Calls) != 1 || response.Calls[0].Name != "moderator_idle" {
		t.Fatalf("expected synthetic moderator_idle, got %+v", response.Calls)
	}
}

// An audit worker that never claims a file must be pushed into reading code
// instead of chatting in the forum forever.
func TestAuditNudgeRestrictsToolsBeforeClaiming(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	team.cfg.Agent.AuditNudgeTurns = 10
	a := newWorker(team.cfg, team.prompts, team.client, team.compressClient, team.registry.Fork(), "audit-1", phaseAudit, team.board)
	defer a.tools.Close()
	a.turn = 12

	if reason := a.toolPermission("forum_read"); reason == "" {
		t.Fatal("forum_read should be blocked before the first file claim")
	}
	if reason := a.toolPermission("read_file"); reason != "" {
		t.Fatalf("read_file must stay available: %s", reason)
	}
	a.fileReviewCalls = 1
	if reason := a.toolPermission("forum_read"); reason != "" {
		t.Fatalf("after claiming a file the guard should lift: %s", reason)
	}
}

// A recon handoff with too few files is rejected: a thin map produces a shallow
// audit (36-file map → 5 findings, 5-file map → 0 findings).
// A thin model map used to be rejected, which only taught the model to retry and
// burn its budget. The scope is now topped up from the deterministic shortlist, so
// the audit stage always has enough high-signal files.
func TestAuditPlanTopsUpThinMap(t *testing.T) {
	team := newTestTeam(t, nil)
	defer team.Close()
	team.createStageLocked(phaseRecon)
	a := team.workers[0].agent

	workspace := t.TempDir()
	// Signal-bearing names: the shortlist only seeds files that look like a risk
	// class, so a fixture of "a.go" would never be topped up.
	for _, name := range []string{"UserController.java", "XssFilter.java", "UploadService.java", "UserDao.java"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := tools.NewRegistry(workspace, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	a.tools.Close()
	a.tools = registry
	a.cfg.Agent.MinAuditFiles = 3

	args, _ := json.Marshal(map[string]any{
		"summary": "s", "audit_map": "m", "execute_instructions": "e",
		"audit_files": []string{"UserController.java"},
	})
	out := a.auditPlanDone(args)
	if !strings.Contains(out, `"ok": true`) {
		t.Fatalf("thin map must still hand off, got %s", out)
	}
	if a.plan == nil || len(a.plan.AuditFiles) != 4 {
		t.Fatalf("thin map must be topped up with every available file, got %+v", a.plan)
	}
	if a.plan.AuditFiles[0] != "UserController.java" {
		t.Fatalf("model pick must stay first, got %v", a.plan.AuditFiles)
	}

	// A map that already meets the target must not be padded further.
	var full []string
	for _, name := range []string{"UserController.java", "XssFilter.java", "UploadService.java", "UserDao.java"} {
		full = append(full, name)
	}
	args, _ = json.Marshal(map[string]any{
		"summary": "s", "audit_map": "m", "execute_instructions": "e",
		"audit_files": full,
	})
	_ = a.auditPlanDone(args)
	if len(a.plan.AuditFiles) != len(full) {
		t.Fatalf("full map must not be padded: %v", a.plan.AuditFiles)
	}
}
