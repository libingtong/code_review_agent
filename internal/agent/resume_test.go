package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/tools"
)

// Reconnaissance is the slow half of a run. ReuseRecon must keep the saved map and
// scope while rebuilding the audit team, so a repeat scan skips reconnaissance.
func TestReuseReconRebuildsAuditStageFromSavedMap(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseRecon)
	for i, w := range team.workers {
		files := []string{"entry1.go", "entry2.go", "entry3.go"}
		w.agent.plan = &auditPlanDoneArgs{Summary: "侦察一起", AuditMap: "入口与鉴权", AuditFiles: files}
		w.agent.completed = true
		w.agent.turn = 40 + i
		team.capture(w, w.agent)
	}
	team.mu.Lock()
	team.phase = phaseAudit
	team.handoff = team.buildHandoffLocked()
	team.createStageLocked(phaseAudit)
	team.mu.Unlock()
	for _, w := range team.workers {
		if w.saved.Status.Phase == phaseAudit {
			w.agent.completed = true
			w.agent.turn = 90
			w.agent.tools.RestoreSnapshot(tools.Snapshot{
				Files:    w.agent.tools.Snapshot().Files,
				Audit:    tools.AuditState{Ended: true, Summary: "上一轮结束"},
				Findings: w.agent.tools.Snapshot().Findings,
			})
			team.capture(w, w.agent)
		}
	}

	path := filepath.Join(t.TempDir(), "session.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, &noticeClient{})
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	if err := restored.ReuseRecon(); err != nil {
		t.Fatal(err)
	}

	if restored.Phase() != phaseAudit {
		t.Fatalf("phase = %s, want audit", restored.Phase())
	}
	var auditWorkers, reconWorkers int
	for _, w := range restored.workers {
		switch w.saved.Status.Phase {
		case phaseAudit:
			auditWorkers++
			if w.agent.completed || w.saved.Completed {
				t.Fatalf("audit worker %s must restart uncompleted", w.agent.id)
			}
			if w.agent.turn != 0 {
				t.Fatalf("audit worker %s kept turn %d", w.agent.id, w.agent.turn)
			}
			if !strings.Contains(w.agent.handoff, "侦察一起") {
				t.Fatalf("audit worker lost the recon handoff")
			}
			statuses := map[string]string{}
			for _, file := range w.agent.tools.Snapshot().Files {
				statuses[file.Path] = file.Status
			}
			for _, want := range []string{"entry1.go", "entry2.go", "entry3.go"} {
				if statuses[want] != "pending" {
					t.Fatalf("scope not rebuilt for %s: %v", want, statuses)
				}
			}
			if len(w.agent.tools.Snapshot().Findings) != 0 {
				t.Fatalf("fresh audit team must not inherit findings")
			}
		case phaseRecon:
			reconWorkers++
		}
	}
	if auditWorkers == 0 || reconWorkers == 0 {
		t.Fatalf("unexpected team layout: audit=%d recon=%d", auditWorkers, reconWorkers)
	}
	coverage := tools.ComputeCoverage(restored.Snapshot())
	if coverage.Total != 3 {
		t.Fatalf("coverage denominator = %d, want the saved scope of 3 files", coverage.Total)
	}
}

// A session without a recon plan cannot be re-scanned from its map.
func TestReuseReconRejectsSessionWithoutMap(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseRecon)
	path := filepath.Join(t.TempDir(), "session.json")
	if err := team.SaveSession(path); err != nil {
		t.Fatal(err)
	}
	restored := newTestTeam(t, &noticeClient{})
	if err := restored.LoadSession(path); err != nil {
		t.Fatal(err)
	}
	if err := restored.ReuseRecon(); err == nil {
		t.Fatal("session without recon plan must be rejected")
	}
}

func TestResetBudgetUsageClearsSavedCounters(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.mu.Lock()
	team.budget.elapsed = 30 * time.Minute
	team.budget.tokens = 123456
	team.mu.Unlock()
	team.ResetBudgetUsage()
	status := team.BudgetStatus()
	if status.Elapsed != 0 || status.UsedTokens != 0 || status.StopReason != "" {
		t.Fatalf("budget not rebased: %+v", status)
	}
}
