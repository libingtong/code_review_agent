package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
)

func TestDuplicateReadRecoveryOnceWhenEvidenceLeavesVisibleHistory(t *testing.T) {
	var g duplicateReadGuard
	full := `{"ok":true,"data":{"path":"a.go","offset":1,"limit":1,"lines":[{"line":1,"text":"evidence"}]}}`
	visible := map[string]bool{}
	inspect := func(id string) (string, string) {
		return g.inspect(llm.FunctionCall{CallID: id, Name: "read_file", Arguments: `{"path":"a.go"}`}, full, "/workspace", func(id string) bool { return visible[id] })
	}
	inspect("first")
	visible["first"] = true
	inspect("second")
	visible["second"] = true
	if replacement, _ := inspect("third"); !strings.Contains(replacement, "重复读取已拦截") {
		t.Fatal(replacement)
	}
	// The first result remains visible: it is specifically the most recent
	// delivered evidence (second), rather than the first call, that is checked.
	delete(visible, "second")
	replacement, notice := inspect("restored")
	if !strings.Contains(replacement, `"ok":true`) || !strings.Contains(replacement, "evidence") || !strings.Contains(notice, "证据恢复") {
		t.Fatal(replacement, notice)
	}
	for _, entry := range g.entries {
		if entry.count != 3 || !entry.recovered || entry.firstCallID != "first" || entry.lastEvidenceID != "restored" {
			t.Fatalf("entry=%+v", entry)
		}
	}
	visible["restored"] = true
	if replacement, _ := inspect("still-visible"); !strings.Contains(replacement, "重复读取已拦截") {
		t.Fatal(replacement)
	}
	delete(visible, "restored")
	if replacement, _ := inspect("compressed-again"); !strings.Contains(replacement, "重复读取已拦截") || strings.Contains(replacement, "evidence") {
		t.Fatal(replacement)
	}
}

func TestDuplicateReadRecoveryNativeIntegration(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	run := func(i int) string {
		call := llm.FunctionCall{CallID: fmt.Sprintf("recovery-%d", i), Name: "read_file", Arguments: `{"path":"entry1.go","limit":1}`}
		a.recordResponse(llm.ToolResponse{Calls: []llm.FunctionCall{call}})
		full := a.executeNativeTool(context.Background(), func(Event) {}, call)
		if err := validateNativeHistory(a.messages); err != nil {
			t.Fatal(err)
		}
		return full
	}
	run(1)
	run(2)
	if full := run(3); !strings.Contains(full, "重复读取已拦截") {
		t.Fatal(full)
	}
	// Simulate compression removing old calls and outputs. New call/output pairs
	// must remain valid while only one recovery is granted across compressions.
	a.messages = nil
	if full := run(4); !strings.Contains(full, "证据恢复") || !strings.Contains(full, `"ok":true`) {
		t.Fatal(full)
	}
	a.messages = nil
	if full := run(5); !strings.Contains(full, "重复读取已拦截") {
		t.Fatal(full)
	}
}
