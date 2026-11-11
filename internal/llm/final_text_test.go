package llm

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestFinalTextOnlyIsRequestScopedForResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			// Literal markers in final content must survive; this is channel selection,
			// not string stripping of model output.
			const answer = "<think>source literal</think> final evidence"
			const response = `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"intermediate"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<think>source literal</think> final evidence"}]}]}`
			c := responsesTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				} else {
					fmt.Fprint(w, response)
				}
			}, streaming, "responses")
			for _, finalOnly := range []bool{true, false, true} {
				ctx := context.Background()
				want := "<think>intermediate</think>" + answer
				if finalOnly {
					ctx = WithFinalTextOnly(ctx)
					want = answer
				}
				got, err := c.Chat(ctx, nil)
				if err != nil || got != want {
					t.Fatalf("finalOnly=%v: got %q error %v, want %q", finalOnly, got, err, want)
				}
			}
		})
	}
}
