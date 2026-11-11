package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

// Compression is an ordinary text request, but must retain the channel's
// streaming transport: healthy generation can last longer than the idle limit.
func TestAuxClientPreservesStreamingPastTotalTimeout(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			requested := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Stream bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				requested <- payload.Stream
				if !payload.Stream {
					if streaming {
						// A non-streaming regression has no output progress before its total
						// HTTP timeout. Do not spend another request lifetime after cancellation.
						select {
						case <-time.After(1400 * time.Millisecond):
						case <-r.Context().Done():
							return
						}
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"message":{"content":"summary"},"finish_reason":"stop"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				for i := 0; i < 7; i++ {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"summary \"},\"finish_reason\":null}]}\n\n")
					flusher.Flush()
					select {
					case <-time.After(200 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				flusher.Flush()
			}))
			defer server.Close()
			source := config.OpenAIConfig{APIInterface: "chat", BaseURL: server.URL + "/v1", APIKey: "test", Model: "fixture", Stream: streaming, TimeoutSeconds: 1, MaxOutputTokens: 100}
			client := auxClient(config.OpenAIConfig{}, source)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			text, err := client.Chat(ctx, []llm.Message{{Role: llm.RoleUser, Content: "compress evidence"}})
			if err != nil {
				t.Fatalf("healthy compression transport failed: %v", err)
			}
			if got := <-requested; got != streaming {
				t.Fatalf("compression stream=%v, source stream=%v", got, streaming)
			}
			expected := "summary"
			if streaming {
				expected = strings.TrimSpace(strings.Repeat("summary ", 7))
			}
			if strings.TrimSpace(text) != expected {
				t.Fatalf("incomplete summary: %q", text)
			}
		})
	}
}
