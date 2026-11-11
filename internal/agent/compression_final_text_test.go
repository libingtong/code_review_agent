package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func TestCompressionKeepsFinalSummaryInsteadOfReasoningPrefix(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			// Large reasoning used to occupy the entire bounded summary preview,
			// dropping the actual evidence and next action from restored history.
			thinking := strings.Repeat("intermediate deliberation; ", 500)
			final := "Evidence: entry.go:41 calls validateCallback; next inspect its signature check. Preserve literal <think> from source."
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, delta := range []map[string]string{{"reasoning_content": thinking}, {"content": final}} {
						b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
						fmt.Fprintf(w, "data: %s\n\n", b)
					}
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"reasoning_content": thinking, "content": final}, "finish_reason": "stop"}}})
				}
			}))
			defer server.Close()
			compressor := llm.NewOpenAIClient(config.OpenAIConfig{APIInterface: "chat", BaseURL: server.URL + "/v1", APIKey: "test", Model: "fixture", Stream: streaming, MaxOutputTokens: 10000})
			a := tokenBudgetWorker(t, nil, compressor)
			a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "audit"}, {Role: llm.RoleUser, Content: "Source evidence to retain"}}
			if err := a.compressContext(context.Background(), func(Event) {}, "test final summary"); err != nil {
				t.Fatal(err)
			}
			var summary tools.Result
			if err := json.Unmarshal([]byte(strings.TrimPrefix(a.messages[1].Content, "Compressed audit context:\n")), &summary); err != nil {
				t.Fatal(err)
			}
			if summary.Trunc || summary.Data != final {
				t.Fatalf("final evidence lost: truncated=%v retained_final=%v", summary.Trunc, summary.Data == final)
			}
		})
	}
}
