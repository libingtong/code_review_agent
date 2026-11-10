package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestAttemptMeterCorrectsEstimatedUsage(t *testing.T) {
	tests := []struct {
		name    string
		updates []llm.UsageUpdate
		want    int64
	}{
		{
			name: "downward provider correction",
			updates: []llm.UsageUpdate{
				{RequestID: 1, TotalTokens: 1000, Estimated: true},
				{RequestID: 1, TotalTokens: 100},
				{RequestID: 1, TotalTokens: 100, Final: true},
			},
			want: 100,
		},
		{
			name: "interleaved requests and repeated final",
			updates: []llm.UsageUpdate{
				{RequestID: 1, TotalTokens: 1000, Estimated: true},
				{RequestID: 2, TotalTokens: 500, Estimated: true},
				{RequestID: 1, TotalTokens: 100, Final: true},
				{RequestID: 2, TotalTokens: 700, Final: true},
				{RequestID: 1, TotalTokens: 100, Final: true},
			},
			want: 800,
		},
		{
			name: "zero correction and invalid negative update",
			updates: []llm.UsageUpdate{
				{RequestID: 1, TotalTokens: 1000, Estimated: true},
				{RequestID: 1, TotalTokens: -1},
				{RequestID: 1, TotalTokens: 0, Final: true},
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var meter attemptMeter
			for _, update := range tt.updates {
				meter.Add(update)
			}
			if got := meter.Take(); got != tt.want {
				t.Fatalf("usage = %d, want corrected usage %d", got, tt.want)
			}
			if got := meter.Take(); got != 0 {
				t.Fatalf("second Take = %d, want 0", got)
			}
		})
	}
}

func TestPoolObserveCommitsOnlySuccessfulCorrectedUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":80,"completion_tokens":20,"total_tokens":100}}`))
	}))
	defer server.Close()
	entry := testProvider("a", server.URL, 10000)
	entry.Quota.Local.Unit = "tokens"
	pool, err := NewPool(testConfig(entry))
	if err != nil {
		t.Fatal(err)
	}
	client := llm.NewOpenAIClient(entry.OpenAI)
	messages := []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("x", 4000)}}
	for _, commit := range []bool{false, true} {
		ctx, finish := pool.observe(context.Background(), "a")
		if _, err := client.Chat(ctx, messages); err != nil {
			t.Fatal(err)
		}
		finish(commit)
		status := pool.find("a").quota.Status(pool.now())
		if !commit {
			if status.Used != 0 || !status.ResetAt.IsZero() {
				t.Fatalf("discarded attempt started/charged quota: %+v", status)
			}
		} else if status.Used != 100 {
			t.Fatalf("committed usage = %v, want provider total 100", status.Used)
		}
	}
}
