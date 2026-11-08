package tools

import (
	"fmt"
	"strings"
	"time"
)

// Coverage is the progress picture a user actually asks for: how many files are
// in scope, how many are done, what is left, and how long that should take.
type Coverage struct {
	Total     int
	Reviewed  int
	Reviewing int
	Pending   int
	Skipped   int
	Percent   float64
	Remaining int
	// Candidates is the workspace pool; shown before an audit scope exists so the
	// operator can judge scale ("0/0" alone tells them nothing).
	Candidates int
}

// ComputeCoverage derives coverage from the review registry. "Skipped" files are
// out of scope for the current audit, so they count toward neither side.
func ComputeCoverage(snapshot Snapshot) Coverage {
	var coverage Coverage
	for _, file := range snapshot.Files {
		switch file.Status {
		case "skipped":
			coverage.Skipped++
			continue
		}
		coverage.Total++
		switch file.Status {
		case "reviewed", "done", "complete":
			coverage.Reviewed++
		case "reviewing", "in_progress":
			coverage.Reviewing++
		}
	}
	coverage.Candidates = snapshot.Candidates
	coverage.Remaining = coverage.Total - coverage.Reviewed
	if coverage.Total > 0 {
		coverage.Percent = float64(coverage.Reviewed) * 100 / float64(coverage.Total)
	}
	return coverage
}

// ETA estimates the remaining time from the observed review rate. It returns 0
// when there is not enough progress to extrapolate (no file reviewed yet).
func (c Coverage) ETA(elapsed time.Duration) time.Duration {
	if c.Reviewed == 0 || c.Remaining == 0 || elapsed <= 0 {
		return 0
	}
	perFile := elapsed / time.Duration(c.Reviewed)
	return perFile * time.Duration(c.Remaining)
}

// String renders the one-line summary used by the TUI header and headless logs.
func (c Coverage) String(elapsed time.Duration) string {
	if c.Total == 0 && c.Candidates > 0 {
		return fmt.Sprintf("覆盖 0/0（待认领）· 候选 %d 个文件", c.Candidates)
	}
	line := fmt.Sprintf("覆盖 %d/%d（%.0f%%）· 剩余 %d", c.Reviewed, c.Total, c.Percent, c.Remaining)
	if c.Reviewing > 0 {
		line += fmt.Sprintf(" · 进行中 %d", c.Reviewing)
	}
	if eta := c.ETA(elapsed); eta > 0 {
		line += " · 预计还需 " + HumanDuration(eta)
	}
	return line
}

// HumanDuration formats a duration the way an operator reads it (12m, 1h05m).
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%02dm", hours, minutes)
}

// CoverageLine is a convenience for callers that only have a snapshot.
func CoverageLine(snapshot Snapshot, elapsed time.Duration) string {
	return ComputeCoverage(snapshot).String(elapsed)
}

// FileStatusCounts summarises statuses for diagnostics.
func FileStatusCounts(snapshot Snapshot) string {
	counts := map[string]int{}
	for _, file := range snapshot.Files {
		status := strings.TrimSpace(file.Status)
		if status == "" {
			status = "unknown"
		}
		counts[status]++
	}
	parts := make([]string, 0, len(counts))
	for status, count := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", status, count))
	}
	return strings.Join(parts, " ")
}
