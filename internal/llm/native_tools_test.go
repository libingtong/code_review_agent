package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/config"
)

func responseCallEvent(kind, args string) string {
	return fmt.Sprintf("data: {\"type\":%q,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_real\",\"call_id\":\"call_real\",\"name\":\"read_file\",\"arguments\":%q}}\n\n", kind, args)
}
func TestNativeResponsesNeverReleaseInvalidOrUnfinishedCalls(t *testing.T) {
	start := responseCallEvent("response.output_item.added", "")
	complete := `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	for _, tc := range []struct {
		name, events string
		protocol     bool
	}{
		{"EOF after complete JSON", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{}"}` + "\n\n", false},
		{"cancelled terminal", start + responseCallEvent("response.output_item.done", `{}`) + `data: {"type":"response.cancelled","response":{"status":"cancelled"}}` + "\n\n", false},
		{"mismatched item identity", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_other","delta":"{}"}` + "\n\n" + complete, true},
		{"snapshot replaces generated arguments", start + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{\"path\":\"good\"}"}` + "\n\n" + responseCallEvent("response.output_item.done", `{"path":"different"}`) + complete, true},
		{"missing call ID", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"fc_not_a_call_id","name":"read_file","arguments":"{}"}]}}` + "\n\n", true},
		{"array arguments", start + responseCallEvent("response.output_item.done", `[]`) + complete, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := readNativeResponses(context.Background(), strings.NewReader(tc.events), nil)
			if err == nil || len(result.Calls) != 0 || result.Content != "" || result.Thinking != "" {
				t.Fatalf("invalid stream exposed executable output: %+v %v", result, err)
			}
			if tc.protocol && !errors.Is(err, ErrToolProtocol) {
				t.Fatalf("model format error not classifiable: %v", err)
			}
		})
	}
}
func TestNativeResponsesSnapshotDeduplicatesCallsAndKeepsReasoningInert(t *testing.T) {
	examples := `data: {"type":"response.reasoning_text.delta","output_index":1,"content_index":0,"delta":"<tool_call>dangerous example</tool_call>"}` + "\n\n"
	events := examples + responseCallEvent("response.output_item.added", "") + `data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"{\"path\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_real","delta":"\"a.go\"}"}` + "\n\n" + responseCallEvent("response.output_item.done", `{"path":"a.go"}`) +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","id":"fc_real","call_id":"call_real","name":"read_file","arguments":"{\"path\":\"a.go\"}"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"<tool_call>dangerous example</tool_call>"}]}]}}` + "\n\n"
	result, err := readNativeResponses(context.Background(), strings.NewReader(events), nil)
	if err != nil || len(result.Calls) != 1 || result.Calls[0].CallID != "call_real" || result.Calls[0].Arguments != `{"path":"a.go"}` || result.Thinking != "<tool_call>dangerous example</tool_call>" {
		t.Fatalf("stream/snapshot mismatch: %+v %v", result, err)
	}
}
func TestNativeChatRequiresSuccessfulTerminalAndStableCallIdentity(t *testing.T) {
	start := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"real","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}` + "\n\n"
	for _, ending := range []string{
		"data: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\ndata: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"changed","function":{"arguments":""}}]}}]}` + "\n\ndata: [DONE]\n\n",
	} {
		result, err := readNativeChat(context.Background(), strings.NewReader(start+ending), nil)
		if err == nil || len(result.Calls) != 0 {
			t.Fatalf("unfinished/ambiguous call accepted: %+v %v", result, err)
		}
	}
	result, err := readNativeChat(context.Background(), strings.NewReader(start), nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(result.Calls) != 0 {
		t.Fatalf("unterminated chat call exposed: %+v %v", result, err)
	}
}
func TestNativeToolStreamCancellationDiscardsCollectedCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := responseCallEvent("response.output_item.added", `{}`) + `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	result, err := readNativeResponses(ctx, strings.NewReader(events), func(d Delta) error {
		if d.Content != "" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
		t.Fatalf("cancellation released tool call: %+v %v", result, err)
	}
}

func TestNativeProgressArrivesBeforeTerminalAndUsesProviderUsage(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			advance := make(chan struct{})
			progress := make(chan GenerationProgress, 16)
			finished := make(chan error, 1)
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(event string) { fmt.Fprintf(w, "data: %s\n\n", event); w.(http.Flusher).Flush() }
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.reasoning_text.delta","delta":"abcdefghijklmnop"}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{"reasoning_content":"abcdefghijklmnop"}}]}`)
				}
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				fmt.Fprint(w, ": keepalive\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`)
				}
				select {
				case <-advance:
				case <-ctx.Done():
					return
				}
				if api == "responses" {
					send(`{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1}},"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"abcdefghijklmnop"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}}`)
				} else {
					send(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
					send(`{"choices":[],"usage":{"completion_tokens":3,"completion_tokens_details":{"reasoning_tokens":1}}}`)
					send(`[DONE]`)
				}
			}, true, api)
			go func() {
				_, err := client.ChatTools(ctx, nil, nil, func(d Delta) error {
					if d.Progress != nil {
						progress <- *d.Progress
					}
					return nil
				})
				finished <- err
			}()
			next := func() GenerationProgress {
				t.Helper()
				select {
				case p := <-progress:
					return p
				case <-ctx.Done():
					t.Fatal("timed out waiting for progress")
					return GenerationProgress{}
				}
			}
			step := func() {
				t.Helper()
				select {
				case advance <- struct{}{}:
				case <-ctx.Done():
					t.Fatal("stream did not advance")
				}
			}
			initial := next()
			if initial.OutputTokens != 0 || !initial.Estimated || initial.ReceivedAt != "" {
				t.Fatalf("reset attributed output before network data: %+v", initial)
			}
			step()
			first := next()
			if first.OutputTokens != 4 || first.ReasoningTokens != 4 || !first.Estimated || first.ReceivedAt == "" {
				t.Fatalf("first reasoning not visible: %+v", first)
			}
			step()
			select {
			case p := <-progress:
				t.Fatalf("heartbeat generated progress: %+v", p)
			case <-time.After(300 * time.Millisecond):
			}
			step()
			second := next()
			if second.OutputTokens != 5 || second.ToolTokens != 1 || second.ReceivedAt == first.ReceivedAt {
				t.Fatalf("ongoing tool output not counted: %+v", second)
			}
			select {
			case err := <-finished:
				t.Fatalf("request ended before terminal: %v", err)
			default:
			}
			step()
			final := next()
			if final.Estimated || final.OutputTokens != 3 || final.ReasoningTokens != 1 || final.ToolTokens != 0 || final.ReceivedAt != second.ReceivedAt {
				t.Fatalf("provider usage did not replace estimates or snapshot duplicated output: %+v", final)
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("request did not finish")
			}
		})
	}
}

func TestNativeResponseProgressRecoversSnapshotsWithoutDuplicateTokens(t *testing.T) {
	events := `data: {"type":"response.output_text.delta","output_index":0,"delta":"ab"}` + "\n\n" +
		`data: {"type":"response.output_text.done","output_index":0,"text":"abcd"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"12345678"}]}}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"abcd"}]},{"type":"reasoning","content":[{"type":"reasoning_text","text":"12345678"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}}` + "\n\n"
	var final GenerationProgress
	result, err := readNativeResponses(context.Background(), strings.NewReader(events), func(d Delta) error {
		if d.Progress != nil {
			final = *d.Progress
		}
		return nil
	})
	if err != nil || result.Content != "abcd" || result.Thinking != "12345678" || len(result.Calls) != 1 {
		t.Fatalf("snapshot recovery: %+v, %v", result, err)
	}
	if !final.Estimated || final.OutputTokens != 4 || final.ReasoningTokens != 2 || final.ToolTokens != 1 {
		t.Fatalf("snapshots double counted: %+v", final)
	}
}

func TestNativeProgressCancellationNeverReturnsExecutableCalls(t *testing.T) {
	for _, authoritative := range []bool{false, true} {
		for _, api := range []string{"responses", "chat_completions"} {
			t.Run(fmt.Sprintf("%s/final=%t", api, authoritative), func(t *testing.T) {
				events := responseCallEvent("response.output_item.done", "{}") + `data: {"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":1}}}` + "\n\n"
				reader := readNativeResponses
				if api == "chat_completions" {
					reader = readNativeChat
					events = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"
				}
				result, err := reader(context.Background(), strings.NewReader(events), func(d Delta) error {
					if d.Progress != nil && d.Progress.OutputTokens > 0 && d.Progress.Estimated != authoritative {
						return context.Canceled
					}
					return nil
				})
				if !errors.Is(err, context.Canceled) || len(result.Calls) != 0 {
					t.Fatalf("progress cancellation exposed calls: %+v %v", result, err)
				}
			})
		}
	}
}

func TestNativeNonstreamGenerationUsesSnapshotAndProviderUsage(t *testing.T) {
	for _, api := range []string{"responses", "chat_completions"} {
		t.Run(api, func(t *testing.T) {
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if api == "responses" {
					fmt.Fprint(w, `{"status":"completed","usage":{"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2}},"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"thought"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}]}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"content":"answer","reasoning_content":"thought","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}],"usage":{"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":2}}}`)
				}
			}, false, api)
			var updates []GenerationProgress
			result, err := client.ChatTools(context.Background(), nil, nil, func(d Delta) error {
				if d.Progress != nil {
					updates = append(updates, *d.Progress)
				}
				return nil
			})
			if err != nil || result.Content != "answer" || result.Thinking != "thought" || len(result.Calls) != 1 {
				t.Fatalf("snapshot response: %+v, %v", result, err)
			}
			if len(updates) < 2 {
				t.Fatal("missing generation reset/final snapshot")
			}
			first, final := updates[0], updates[len(updates)-1]
			if first.OutputTokens != 0 || first.ReceivedAt != "" || final.Estimated || final.OutputTokens != 7 || final.ReasoningTokens != 2 || final.ReceivedAt == "" {
				t.Fatalf("invalid snapshot usage transition: %+v", updates)
			}
		})
	}
}

// Without tool_choice the model may answer in prose; the agent layer then has to
// reject the turn. The chat protocol must force a native call.
func TestChatToolsForcesToolChoice(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "chat",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if _, err := client.ChatTools(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d", len(bodies))
	}
	if got := bodies[0]["tool_choice"]; got != "required" {
		t.Fatalf("tool_choice = %v, want required", got)
	}
}

// An endpoint that rejects tool_choice:"required" must be retried with "auto"
// and remembered, instead of failing every turn.
func TestChatToolsFallsBackWhenToolChoiceRejected(t *testing.T) {
	var choices []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		choice, _ := body["tool_choice"].(string)
		choices = append(choices, choice)
		if choice == "required" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter tool_choice"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "chat",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	for i := 0; i < 2; i++ {
		if _, err := client.ChatTools(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, tools, func(Delta) error { return nil }); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if len(choices) == 0 || choices[0] != "required" {
		t.Fatalf("first attempt should force a call: %v", choices)
	}
	if choices[len(choices)-1] != "auto" {
		t.Fatalf("fallback not remembered: %v", choices)
	}
	// 3 requests total: required (400) → auto (ok) → auto (ok)
	if len(choices) != 3 {
		t.Fatalf("requests = %v", choices)
	}
}

func TestProbeToolCallDetectsTextOnlyModel(t *testing.T) {
	prose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好的，我会调用 noop 工具。"}}]}`))
	}))
	defer prose.Close()
	textOnly := NewOpenAIClient(config.OpenAIConfig{BaseURL: prose.URL, APIKey: "k", Model: "m", APIInterface: "chat",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5})
	ok, preview, err := textOnly.ProbeToolCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("prose answer must not count as tool support")
	}
	if !strings.Contains(preview, "noop") {
		t.Fatalf("preview should quote the answer: %q", preview)
	}

	caller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"noop","arguments":"{}"}}]}}]}`))
	}))
	defer caller.Close()
	withTools := NewOpenAIClient(config.OpenAIConfig{BaseURL: caller.URL, APIKey: "k", Model: "m", APIInterface: "chat",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5})
	if ok, _, err := withTools.ProbeToolCall(context.Background()); err != nil || !ok {
		t.Fatalf("native call not detected: ok=%v err=%v", ok, err)
	}
}

// GLM's Responses endpoint hands out ids but rejects previous_response_id. The
// client must drop the lineage and resend once, then stop asking.
func TestResponsesContinuationFallsBackWhenRejected(t *testing.T) {
	var previous []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		prev, _ := body["previous_response_id"].(string)
		previous = append(previous, prev)
		if prev != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"previous_response_id not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_new","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	messages := []Message{{Role: RoleUser, Content: "hi"}}
	ctx := WithResponsesContinuation(context.Background(), "resp_old", messages)

	response, err := client.ChatTools(ctx, messages, tools, func(Delta) error { return nil })
	if err != nil {
		t.Fatalf("continuation rejection must be recoverable: %v", err)
	}
	if len(response.Calls) != 1 {
		t.Fatalf("calls = %d", len(response.Calls))
	}
	// first attempt carried the id and was rejected, retry did not.
	if len(previous) != 2 || previous[0] != "resp_old" || previous[1] != "" {
		t.Fatalf("retry did not drop the continuation: %v", previous)
	}

	// The capability is remembered: a later turn does not send it at all.
	if _, err := client.ChatTools(ctx, messages, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if last := previous[len(previous)-1]; last != "" {
		t.Fatalf("continuation still sent after fallback: %q", last)
	}
	if len(previous) != 3 {
		t.Fatalf("unexpected request count: %v", previous)
	}
}

// MiniMax honours previous_response_id, so the optimisation must stay enabled.
func TestResponsesContinuationIsKeptWhenSupported(t *testing.T) {
	var previous []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		prev, _ := body["previous_response_id"].(string)
		previous = append(previous, prev)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_2","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	messages := []Message{{Role: RoleUser, Content: "hi"}}
	ctx := WithResponsesContinuation(context.Background(), "resp_1", messages)
	if _, err := client.ChatTools(ctx, messages, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(previous) != 1 || previous[0] != "resp_1" {
		t.Fatalf("supported continuation must be sent: %v", previous)
	}
}

// MiniMax ignores tool_choice:"required"; the per-request instructions directive
// is what actually forces a native call there.
func TestResponsesToolsSendToolDirective(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if _, err := client.ChatTools(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	instructions, _ := body["instructions"].(string)
	if !strings.Contains(instructions, "禁止只用文字回答") {
		t.Fatalf("tool directive missing: %q", instructions)
	}
	if choice, _ := body["tool_choice"].(string); choice != "required" {
		t.Fatalf("tool_choice = %q", choice)
	}
}

// Some Responses endpoints accept tools but reject the replayed function_call
// items with "invalid_request ... messages". The client must retry once with a
// text-degraded history and remember the mode for later turns.
func TestResponsesFallsBackToTextHistory(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if strings.Contains(mustJSON(body["input"]), `"type":"function_call"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"messages 参数非法。请检查文档。"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_ok","status":"completed","output":[{"type":"function_call","call_id":"c2","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	history := []Message{
		{Role: RoleUser, Content: "调用 noop"},
		{Type: "function_call", CallID: "c1", Name: "noop", Arguments: "{}"},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
		{Role: RoleUser, Content: "继续"},
	}
	if _, err := client.ChatTools(context.Background(), history, tools, func(Delta) error { return nil }); err != nil {
		t.Fatalf("fallback must recover: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2 (native then degraded)", len(bodies))
	}
	if !strings.Contains(mustJSON(bodies[0]["input"]), `"function_call"`) {
		t.Fatalf("first attempt should use native items")
	}
	degraded := mustJSON(bodies[1]["input"])
	if strings.Contains(degraded, `"type":"function_call"`) {
		t.Fatalf("degraded history still has native items: %s", degraded)
	}
	if !strings.Contains(degraded, "工具结果") {
		t.Fatalf("degraded history should carry the tool result: %s", degraded)
	}

	// Subsequent turns go straight to the degraded shape.
	if _, err := client.ChatTools(context.Background(), history, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || strings.Contains(mustJSON(bodies[2]["input"]), `"type":"function_call"`) {
		t.Fatalf("text history not remembered: %v", mustJSON(bodies[2]["input"]))
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// GLM can reject a replayed history mid-stream with response.failed. When nothing
// was emitted yet, the same turn is retried with text history instead of killing
// the agent.
func TestStreamedHistoryRejectionRetriesSameTurn(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(mustJSON(body["input"]), `"type":"function_call"`) {
			_, _ = w.Write([]byte("event: response.failed\n"))
			_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_request","message":"messages 参数非法。请检查文档。"}}}` + "\n\n"))
			return
		}
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c9","name":"noop","arguments":"{}"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[{"type":"function_call","call_id":"c9","name":"noop","arguments":"{}"}]}}` + "\n\n"))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: true})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	history := []Message{
		{Role: RoleUser, Content: "看一下"},
		{Type: "function_call", CallID: "c1", Name: "noop", Arguments: "{}"},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
		{Role: RoleUser, Content: "继续"},
	}
	response, err := client.ChatTools(context.Background(), history, tools, func(Delta) error { return nil })
	if err != nil {
		t.Fatalf("same-turn retry must recover: %v", err)
	}
	if len(response.Calls) != 1 || response.Calls[0].CallID != "c9" {
		t.Fatalf("calls = %+v", response.Calls)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want native then degraded", len(requests))
	}
	if !strings.Contains(mustJSON(requests[0]["input"]), `"function_call"`) {
		t.Fatal("first attempt should replay native items")
	}
	if strings.Contains(mustJSON(requests[1]["input"]), `"type":"function_call"`) {
		t.Fatalf("retry should use text history: %s", mustJSON(requests[1]["input"]))
	}
}

// history: text must skip native replays from the very first request.
func TestTextHistoryModeNeverReplaysNativeItems(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()

	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false, History: "text"})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	history := []Message{
		{Role: RoleUser, Content: "看文件"},
		{Type: "function_call", CallID: "c1", Name: "noop", Arguments: "{}"},
		{Type: "function_call_output", CallID: "c1", Content: `{"ok":true}`},
	}
	if _, err := client.ChatTools(context.Background(), history, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1 (no retry needed)", len(bodies))
	}
	input := mustJSON(bodies[0]["input"])
	if strings.Contains(input, `"type":"function_call"`) || strings.Contains(input, `"type":"function_call_output"`) {
		t.Fatalf("text history mode still replayed native items: %s", input)
	}
	if !strings.Contains(input, "工具结果") {
		t.Fatalf("degraded text missing: %s", input)
	}
}

// When a generation is truncated by max_output_tokens, the retry must spend less
// on reasoning instead of replaying the same oversized request.
func TestReducedReasoningDowngradesRequest(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"noop","arguments":"{}"}]}`))
	}))
	defer server.Close()
	client := NewOpenAIClient(config.OpenAIConfig{BaseURL: server.URL, APIKey: "k", Model: "m", APIInterface: "responses",
		MaxContextTokens: 1000, MaxOutputTokens: 100, TopP: 1, TimeoutSeconds: 5, Stream: false,
		Thinking: &config.ThinkingConfig{Type: "enabled"}, ReasoningEffort: "high"})
	tools := []ToolDefinition{{Type: "function", Name: "noop", Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}
	ctx := WithReducedReasoning(context.Background())
	if _, err := client.ChatTools(ctx, []Message{{Role: RoleUser, Content: "hi"}}, tools, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if body["thinking"] != nil {
		t.Fatalf("thinking must be dropped when reasoning is reduced: %v", body["thinking"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("reasoning effort = %v", reasoning)
	}
}
