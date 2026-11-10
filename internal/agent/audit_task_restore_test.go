package agent

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func TestAuditFileTasksDoNotRestartTeamNegotiation(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.auditScope = []string{"entry1.go", "entry2.go", "entry3.go", "entry4.go"}
	team.createStageLocked(phaseAudit)
	for _, w := range team.workers {
		a := w.agent
		if len(a.assigned) == 0 {
			t.Fatal("missing audit slice")
		}
		assertAuditTaskPrompt(t, a.systemPrompt())
		a.reinforceAssignment()
		if !strings.Contains(lastAssignmentDirective(a.messages), a.assigned[0]) {
			t.Fatal("missing concrete unfinished file")
		}
	}
	if !strings.Contains(stageAssignment(phaseRecon, 4), "先调用 forum_roster") {
		t.Fatal("recon collaboration changed")
	}
}

func TestResumeAuditTasksRestoreUnfinishedFilesWithoutChangingConclusions(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	w := team.workers[0]
	a := w.agent
	a.turn = 50
	snapshot := a.tools.Snapshot()
	snapshot.Files = []tools.FileReview{
		{Path: "entry1.go", Status: "reviewed", Note: "source conclusion"},
		{Path: "entry2.go", Status: "reviewing"},
		{Path: "entry3.go", Status: "skipped", Note: "generated"},
		{Path: "entry4.go", Status: "unseen"},
	}
	snapshot.Todos = []tools.Todo{{Title: "finish concrete source task", Status: "in_progress"}}
	a.tools.RestoreSnapshot(snapshot)
	a.messages = []llm.Message{{Role: llm.RoleSystem, Content: "obsolete"}, {Role: llm.RoleUser, Content: "retained evidence"}}
	team.capture(w, a)
	p := filepath.Join(t.TempDir(), "resume.json")
	if err := team.SaveSession(p); err != nil {
		t.Fatal(err)
	}
	if err := team.LoadSession(p); err != nil {
		t.Fatal(err)
	}
	restored := team.workers[0].agent
	if want := []string{"entry2.go", "entry4.go"}; !reflect.DeepEqual(restored.assigned, want) {
		t.Fatalf("assigned=%v want=%v", restored.assigned, want)
	}
	assertAuditTaskPrompt(t, restored.messages[0].Content)
	if !strings.Contains(restored.assignment, "恢复") {
		t.Fatal("resume assignment omitted recovery context")
	}
	restored.reinforceAssignment()
	hint := lastAssignmentDirective(restored.messages)
	if !strings.Contains(hint, "entry2.go") || strings.Contains(hint, "entry1.go") || strings.Contains(hint, "entry3.go") {
		t.Fatalf("bad reminder: %s", hint)
	}
	if got := restored.tools.Snapshot(); !reflect.DeepEqual(got.Files, snapshot.Files) || !reflect.DeepEqual(got.Todos, snapshot.Todos) {
		t.Fatal("restoring tasks changed conclusions/todos")
	}
	if !strings.Contains(restored.messages[1].Content, "retained evidence") {
		t.Fatal("lost historical evidence")
	}
}

func assertAuditTaskPrompt(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"先调用 forum_roster", "先调用 read_handoff", "这是全新的审计团队", "系统不预设角色、主题或文件范围", "不要假设系统预先分配了任何文件"} {
		if strings.Contains(text, bad) {
			t.Fatalf("audit task contradicts assigned work: %s", bad)
		}
	}
}
