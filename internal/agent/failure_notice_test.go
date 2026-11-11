package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestReceiveFailureNoticeHonorsModeratorConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		moderatorEnabled bool
		wantNotice       bool
	}{
		{name: "disabled", moderatorEnabled: false, wantNotice: false},
		{name: "enabled", moderatorEnabled: true, wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team := newTestTeam(t, nil)
			if tc.moderatorEnabled {
				team.cfg.Agent.ModeratorEnabled = &tc.moderatorEnabled
				team.resetBoard()
			}

			a := &Agent{
				id:     "audit-1",
				phase:  phaseAudit,
				board:  team.board,
				runErr: errors.New("fixture worker failure"),
			}
			w := &teamWorker{
				agent: a,
				saved: workerSession{Status: WorkerStatus{ID: a.id, Phase: a.phase, Status: "running"}},
			}
			var events []Event
			team.emitter = func(e Event) { events = append(events, e) }

			team.receive(w, Event{Kind: "error", Content: "fixture worker failure"})

			if got := w.saved.Status.Status; got != "failed" {
				t.Fatalf("worker status=%q, want failed", got)
			}
			var notices int
			for _, message := range team.board.Messages() {
				if message.Topic == "Agent 失败待诊断" {
					notices++
				}
			}
			if got := notices > 0; got != tc.wantNotice {
				t.Fatalf("failure notice present=%v, want %v; messages=%+v", got, tc.wantNotice, team.board.Messages())
			}
			for _, event := range events {
				if strings.Contains(event.Content, "未知接收 Agent") || strings.Contains(event.Content, "无法发布成员失败通知") {
					t.Fatalf("unexpected secondary notification error: %+v", event)
				}
			}
		})
	}
}
