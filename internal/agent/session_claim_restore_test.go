package agent

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func TestSessionResumeRestoresAuditClaimGateAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []tools.FileReview
		claimed bool
	}{
		{name: "reviewing", files: []tools.FileReview{{Path: "entry1.go", Status: "reviewing"}}, claimed: true},
		{name: "reviewed", files: []tools.FileReview{{Path: "entry1.go", Status: "reviewed", Note: "source checked"}}, claimed: true},
		{name: "in progress", files: []tools.FileReview{{Path: "entry1.go", Status: "in_progress"}}, claimed: true},
		{name: "skipped", files: []tools.FileReview{{Path: "entry1.go", Status: "skipped", Note: "generated source"}}, claimed: true},
		{name: "empty"},
		{name: "unseen", files: []tools.FileReview{{Path: "entry1.go", Status: "unseen"}}},
		{name: "pending", files: []tools.FileReview{{Path: "entry1.go", Status: "pending"}}},
		{name: "inventory only", files: []tools.FileReview{{Path: "entry1.go", Status: "unseen"}, {Path: "entry2.go", Status: "pending"}, {Path: "entry3.go"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			team := newTestTeam(t, &noticeClient{})
			team.createStageLocked(phaseAudit)
			worker := team.workers[0]
			a := worker.agent
			a.turn = 42
			snapshot := a.tools.Snapshot()
			snapshot.Files = tc.files
			snapshot.Findings = []tools.Finding{{Title: "Retained candidate", Path: "entry1.go", Line: 1, Evidence: "retained source evidence"}}
			a.tools.RestoreSnapshot(snapshot)
			preserved := []llm.Message{
				{Role: llm.RoleSystem, Content: "obsolete system prompt"},
				{Role: llm.RoleUser, Content: "continue saved task"},
				{Role: llm.RoleAssistant, Content: "retained analysis"},
				{Type: "function_call", CallID: "old-read", Name: "read_file", Arguments: `{"path":"entry1.go"}`},
				{Type: "function_call_output", CallID: "old-read", Content: `{"ok":true,"data":"old evidence"}`},
			}
			a.messages = append([]llm.Message(nil), preserved...)
			team.capture(worker, a)
			path := filepath.Join(t.TempDir(), "resume.json")
			if err := team.SaveSession(path); err != nil {
				t.Fatal(err)
			}
			team.prompts.System = "current audit system prompt"
			if err := team.LoadSession(path); err != nil {
				t.Fatal(err)
			}
			restored := team.workers[0].agent
			if (restored.fileReviewCalls > 0) != tc.claimed {
				t.Fatalf("restored claim count=%d claimed=%v", restored.fileReviewCalls, tc.claimed)
			}
			for _, tool := range []string{"forum_read", "forum_threads", "read_handoff", "review_state"} {
				permission := restored.toolPermission(tool)
				if (permission == "") != tc.claimed {
					t.Fatalf("tool %s permission=%q claimed=%v", tool, permission, tc.claimed)
				}
				declared := false
				for _, definition := range restored.toolDefinitions() {
					if definition.Name == tool {
						declared = true
					}
				}
				if declared != tc.claimed {
					t.Fatalf("tool %s declared=%v claimed=%v", tool, declared, tc.claimed)
				}
			}
			if restored.messages[0].Role != llm.RoleSystem || !strings.Contains(restored.messages[0].Content, "current audit system prompt") || strings.Contains(restored.messages[0].Content, "obsolete system prompt") {
				t.Fatal("system prompt was not refreshed")
			}
			if !reflect.DeepEqual(restored.messages[1:len(preserved)], preserved[1:]) {
				t.Fatal("restore changed retained conversation/tool evidence")
			}
			got := restored.tools.Snapshot()
			if !reflect.DeepEqual(got.Findings, snapshot.Findings) || len(got.Files) != len(snapshot.Files) || (len(got.Files) > 0 && !reflect.DeepEqual(got.Files, snapshot.Files)) {
				t.Fatal("restore changed findings/file conclusions")
			}
			hint := restored.messages[len(restored.messages)-1].Content
			for _, fragment := range []string{"本轮原生工具定义", "已保存", "AGENTS.md", "重新初始化"} {
				if !strings.Contains(hint, fragment) {
					t.Fatalf("missing restore hint %q: %s", fragment, hint)
				}
			}
			if strings.Contains(hint, "read_handoff") || strings.Contains(hint, "review_state") {
				t.Fatalf("restore recommended possibly gated tool: %s", hint)
			}
			if err := validateNativeHistory(restored.messages); err != nil {
				t.Fatal(err)
			}
		})
	}
}
