package tools

import (
	"strings"
	"testing"
	"time"
)

func TestComputeCoverageAndETA(t *testing.T) {
	snapshot := Snapshot{Files: []FileReview{
		{Path: "a.go", Status: "reviewed"},
		{Path: "b.go", Status: "reviewed"},
		{Path: "c.go", Status: "reviewing"},
		{Path: "d.go", Status: "pending"},
		{Path: "e.go", Status: "skipped"},
	}}
	coverage := ComputeCoverage(snapshot)
	if coverage.Total != 4 || coverage.Reviewed != 2 || coverage.Reviewing != 1 || coverage.Remaining != 2 || coverage.Skipped != 1 {
		t.Fatalf("coverage = %+v", coverage)
	}
	if coverage.Percent != 50 {
		t.Fatalf("percent = %.1f", coverage.Percent)
	}
	// 2 reviewed + 1 in progress in 10 minutes → 3m20s/file → 2 remaining.
	if eta := coverage.ETA(10 * time.Minute); eta != 6*time.Minute+40*time.Second {
		t.Fatalf("eta = %s", eta)
	}
	line := coverage.String(10 * time.Minute)
	for _, want := range []string{"覆盖 2/4", "50%", "剩余 2", "预计还需 6m"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
	if coverage.ETA(0) != 0 {
		t.Fatal("eta without elapsed must be zero")
	}
}
