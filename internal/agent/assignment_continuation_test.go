package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

// Assignment reminders are replaced before every request. Removing an old
// reminder must not move the continuation boundary past fresh tool evidence.
func TestAssignmentReplacementSendsActualToolOutputToResponses(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "replace reminder"
		if completed {
			name = "remove completed reminder"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body struct {
					PreviousResponseID string `json:"previous_response_id"`
					Input              []struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Output string `json:"output"`
					} `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "bad json", 400)
					return
				}
				found := false
				for _, item := range body.Input {
					if item.Type == "function_call_output" && item.CallID == "read_evidence" {
						found = true
						var result struct {
							OK bool `json:"ok"`
						}
						if err := json.Unmarshal([]byte(item.Output), &result); err != nil || !result.OK || !strings.Contains(item.Output, "entry1.go") {
							t.Errorf("actual source output omitted or changed: %s", item.Output)
						}
					}
				}
				if !found {
					t.Errorf("tool output read_evidence lost at HTTP boundary; previous_response_id=%q input=%+v", body.PreviousResponseID, body.Input)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_next","status":"completed","output":[{"type":"function_call","id":"item_next","call_id":"call_next","name":"read_file","arguments":"{\"path\":\"entry2.go\"}"}]}`))
			}))
			defer server.Close()
			team := newTestTeam(t, nil)
			cfg := team.cfg.OpenAI
			cfg.BaseURL = server.URL
			cfg.APIInterface = "responses"
			cfg.APIKey = "local-fixture"
			cfg.Stream = false
			cfg.TimeoutSeconds = 2
			team.client = llm.NewOpenAIClient(cfg)
			team.createStageLocked(phaseAudit)
			a := team.workers[0].agent
			a.assigned = []string{"entry1.go"}
			a.tools.PlanAuditScope(a.assigned)
			a.sanitizeMessages()
			a.reinforceAssignment()
			response := llm.ToolResponse{ResponseID: "resp_parent", Calls: []llm.FunctionCall{{Name: "read_file", CallID: "read_evidence", Arguments: `{"path":"entry1.go"}`}}}
			a.recordResponse(response)
			a.commitResponseContinuation(response)
			a.executeNativeTool(context.Background(), func(Event) {}, response.Calls[0])
			if completed {
				snapshot := a.tools.Snapshot()
				for i := range snapshot.Files {
					snapshot.Files[i].Status = "reviewed"
				}
				a.tools.RestoreSnapshot(snapshot)
			}
			if _, err := a.chatStreamOnce(context.Background(), func(Event) {}); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}
