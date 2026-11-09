package agent

import (
	"strings"
	"testing"

	"code-review-agent/internal/tools"
)

func TestAggregateProjectNoteDeduplicatesAndBounds(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.phase = phaseAudit
	team.createStageLocked(phaseAudit)
	shared := strings.Repeat("共享审计证据\n", 20000)
	for _, worker := range team.workers {
		worker.saved.Snapshot.Project = tools.ProjectNote{Note: shared}
	}
	got := team.aggregateLocked().Project.Note
	team.mu.Unlock()
	if len(got) > 64*1024 {
		t.Fatalf("aggregated project note exceeded bound: %d", len(got))
	}
	if strings.Count(got, "共享审计证据") > 16000 {
		t.Fatalf("identical worker notes were concatenated: %d occurrences", strings.Count(got, "共享审计证据"))
	}
}
