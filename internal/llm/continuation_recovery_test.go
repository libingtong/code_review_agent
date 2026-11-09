package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
)

// A channel can return response IDs while rejecting previous_response_id. Both
// the recovery request and later turns must replay the full history, not send
// an incremental suffix with its parent response ID removed.
func TestResponsesRejectedContinuationReplaysFullHistory(t *testing.T) {
	for _, mode := range []string{"tools", "tools-stream", "chat", "chat-stream"} {
		for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s/status=%d", mode, status), func(t *testing.T) {
				history := []Message{
					{Role: RoleSystem, Content: "audit identity and assignment"},
					{Role: RoleUser, Content: "audit the assigned source file"},
					{Role: RoleAssistant, Content: "prior analysis"},
					{Role: RoleUser, Content: "continue auditing"},
				}
				delta := history[len(history)-1:]
				stream := mode == "tools-stream" || mode == "chat-stream"
				requests := 0
				client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					requests++
					var body struct {
						PreviousResponseID string               `json:"previous_response_id"`
						Input              []responsesInputItem `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					wantID, wantMessages := "parent-response", delta
					if requests > 1 {
						wantID, wantMessages = "", history
					}
					if body.PreviousResponseID != wantID || !reflect.DeepEqual(body.Input, responsesInput(wantMessages)) {
						t.Errorf("request %d: previous_response_id=%q input=%+v, want ID=%q input=%+v", requests, body.PreviousResponseID, body.Input, wantID, responsesInput(wantMessages))
					}
					if requests == 1 {
						w.WriteHeader(status)
						fmt.Fprint(w, `{"error":{"message":"previous_response_id not found"}}`)
						return
					}
					response := `{"id":"new-response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
					if mode == "tools" || mode == "tools-stream" {
						response = `{"id":"new-response","status":"completed","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"read_file","arguments":"{}","status":"completed"}]}`
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					} else {
						fmt.Fprint(w, response)
					}
				}, stream, "responses")
				tools := []ToolDefinition{{Type: "function", Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}
				ctx := WithResponsesContinuation(context.Background(), "parent-response", delta)
				// The caller may continue providing an incremental context even after
				// this client has learned that the channel rejects continuation.
				for turn := 0; turn < 2; turn++ {
					var err error
					if mode == "tools" || mode == "tools-stream" {
						var response ToolResponse
						response, err = client.ChatTools(ctx, history, tools, nil)
						if err == nil && len(response.Calls) != 1 {
							t.Fatalf("turn %d calls = %+v", turn, response.Calls)
						}
					} else if mode == "chat-stream" {
						err = client.ChatStream(ctx, history, func(Delta) error { return nil })
					} else {
						var answer string
						answer, err = client.Chat(ctx, history)
						if err == nil && answer != "answer" {
							t.Fatalf("turn %d answer = %q", turn, answer)
						}
					}
					if err != nil {
						t.Fatalf("turn %d: %v", turn, err)
					}
					if client.continuationEnabled() {
						t.Fatal("channel still allows continuation after rejection")
					}
				}
				if requests != 3 {
					t.Fatalf("requests = %d, want initial rejection, recovery, next turn", requests)
				}
			})
		}
	}
}

// Cancel after the rejected response is consumed, precisely before the recovery
// request would start. Recovery must not detach from the caller's cancellation.
func TestResponsesContinuationRecoveryHonorsCallerCancellation(t *testing.T) {
	for _, mode := range []string{"tools", "tools-stream", "chat", "chat-stream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := 0
			client := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"message":"previous_response_id not found"}}`)
			}, mode == "tools-stream" || mode == "chat-stream", "responses")
			client.httpClient.Transport = continuationCancelTransport{cancel: cancel}
			history := []Message{{Role: RoleSystem, Content: "audit assignment"}, {Role: RoleUser, Content: "continue"}}
			ctx = WithResponsesContinuation(ctx, "parent-response", history[1:])
			var err error
			if mode == "tools" || mode == "tools-stream" {
				_, err = client.ChatTools(ctx, history, []ToolDefinition{{Type: "function", Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}, nil)
			} else if mode == "chat-stream" {
				err = client.ChatStream(ctx, history, func(Delta) error { return nil })
			} else {
				_, err = client.Chat(ctx, history)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled recovery = %v, want context.Canceled", err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, canceled caller must not send recovery", requests)
			}
		})
	}
}

type continuationCancelTransport struct {
	cancel context.CancelFunc
}

func (tr continuationCancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		resp.Body = continuationCancelBody{ReadCloser: resp.Body, cancel: tr.cancel}
	}
	return resp, err
}

type continuationCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body continuationCancelBody) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}
