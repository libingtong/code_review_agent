package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

func duplicateReadOK(t *testing.T, full string) bool {
	t.Helper()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(full), &result); err != nil {
		t.Fatal(err)
	}
	return result.OK
}

func TestDuplicateReadsWarnBlockAndAllowChangedContent(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	path := filepath.Join(a.tools.Workspace(), "entry1.go")
	if err := os.WriteFile(path, []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := func(args string) string {
		calls++
		call := llm.FunctionCall{CallID: fmt.Sprintf("duplicate-%d", calls), Name: "read_file", Arguments: args}
		a.recordResponse(llm.ToolResponse{Calls: []llm.FunctionCall{call}})
		full := a.executeNativeTool(context.Background(), func(Event) {}, call)
		if err := validateNativeHistory(a.messages); err != nil {
			t.Fatal(err)
		}
		return full
	}
	args := `{"path":"entry1.go","offset":1,"limit":1}`
	first := run(args)
	if !duplicateReadOK(t, first) || strings.Contains(first, "重复读取") {
		t.Fatal(first)
	}
	// Different pagination, file and JSON spelling must not break the first page's counter.
	if full := run(`{"path":"entry1.go","offset":2,"limit":1}`); strings.Contains(full, "重复读取") {
		t.Fatal(full)
	}
	second := run(`{"limit":1,"path":"./entry1.go","offset":1}`)
	if !strings.Contains(second, "重复读取提醒") || !strings.Contains(second, "first") || !strings.Contains(second, "duplicate-1") {
		t.Fatal(second)
	}
	third := run(args)
	if !strings.Contains(third, `"ok":false`) || !strings.Contains(third, "重复读取已拦截") || strings.Contains(third, `"data"`) {
		t.Fatal(third)
	}
	if full := run(args); !strings.Contains(full, "重复读取已拦截") {
		t.Fatal(full)
	}
	// Same byte count, same arguments: changed contents must be delivered again.
	if err := os.WriteFile(path, []byte("fresh\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if full := run(args); !strings.Contains(full, "fresh") || strings.Contains(full, "重复读取") {
		t.Fatal(full)
	}
	for _, f := range a.tools.Snapshot().Files {
		if f.Status == "reviewed" {
			t.Fatal("read guard manufactured completion")
		}
	}
}

func TestDuplicateReadsFailuresAndNewFilesHaveSeparateAllowances(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	call := llm.FunctionCall{Name: "read_file", Arguments: `{"path":"new.go"}`}
	for i := 0; i < 4; i++ {
		call.CallID = fmt.Sprintf("failed-%d", i)
		full := a.executeNativeTool(context.Background(), func(Event) {}, call)
		if strings.Contains(full, "重复读取") || duplicateReadOK(t, full) {
			t.Fatal(full)
		}
	}
	if len(a.duplicateReads.entries) != 0 {
		t.Fatal("failed calls consumed success allowance")
	}
	path := filepath.Join(a.tools.Workspace(), "new.go")
	if err := os.WriteFile(path, []byte("package new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(a.tools.Workspace(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	a.tools = registry
	call.CallID = "successful-retry"
	full := a.executeNativeTool(context.Background(), func(Event) {}, call)
	if !duplicateReadOK(t, full) || strings.Contains(full, "重复读取") {
		t.Fatal(full)
	}
	call.Arguments = `{"path":"entry1.go"}`
	call.CallID = "different-file"
	if full := a.executeNativeTool(context.Background(), func(Event) {}, call); strings.Contains(full, "重复读取") {
		t.Fatal(full)
	}
}

func TestDuplicateReadBufferHasWarningAndCannotExposeBlockedResult(t *testing.T) {
	team := newTestTeam(t, &noticeClient{})
	team.createStageLocked(phaseAudit)
	a := team.workers[0].agent
	registry, err := tools.NewRegistry(a.tools.Workspace(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	a.tools = registry
	if err := os.WriteFile(filepath.Join(a.tools.Workspace(), "entry1.go"), []byte(strings.Repeat("x", 12000)), 0600); err != nil {
		t.Fatal(err)
	}
	var firstBuffer string
	for i := 1; i <= 3; i++ {
		call := llm.FunctionCall{CallID: fmt.Sprintf("buffer-%d", i), Name: "read_file", Arguments: `{"path":"entry1.go"}`}
		a.recordResponse(llm.ToolResponse{Calls: []llm.FunctionCall{call}})
		a.executeNativeTool(context.Background(), func(Event) {}, call)
		output := a.messages[len(a.messages)-1].Content
		if len(output) > 2048 {
			t.Fatalf("unbounded result: %d", len(output))
		}
		var page struct {
			BufferID string `json:"buffer_id"`
			Preview  string `json:"preview"`
		}
		if err := json.Unmarshal([]byte(output), &page); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			firstBuffer = page.BufferID
			if firstBuffer == "" {
				t.Fatal("expected pagination")
			}
		}
		if i == 2 && !strings.Contains(page.Preview, "重复读取提醒") {
			t.Fatal(output)
		}
		if i == 3 && (page.BufferID != "" || !strings.Contains(output, "重复读取已拦截")) {
			t.Fatal(output)
		}
	}
	args, _ := json.Marshal(map[string]string{"buffer_id": firstBuffer})
	output := a.tools.Call("read_tool_buffer", args)
	if duplicateReadOK(t, output) {
		t.Fatalf("blocked content retained: %s", output)
	}
}

func TestDuplicateReadDirectoryAndSearchResults(t *testing.T) {
	for _, name := range []string{"list_files", "search_content", "search_context"} {
		t.Run(name, func(t *testing.T) {
			var g duplicateReadGuard
			args := `{"pattern":"needle","path":"."}`
			if name == "list_files" {
				args = `{}`
			}
			call := llm.FunctionCall{Name: name, CallID: "original", Arguments: args}
			full := `{"ok":true,"data":[{"path":"a.go","size":1}]}`
			if replacement, _ := g.inspect(call, full, "/workspace"); replacement != "" {
				t.Fatal(replacement)
			}
			if name == "list_files" {
				call.Arguments = `{"root":"/workspace/.","limit":500,"max_depth":0,"pattern":"","include_hidden":false}`
			}
			if _, notice := g.inspect(call, full, "/workspace"); !strings.Contains(notice, "第2次") {
				t.Fatal(notice)
			}
			if replacement, _ := g.inspect(call, full, "/workspace"); !strings.Contains(replacement, `"ok":false`) {
				t.Fatal(replacement)
			}
			changed := `{"ok":true,"data":[{"path":"a.go","size":2}]}`
			if replacement, _ := g.inspect(call, changed, "/workspace"); replacement != "" {
				t.Fatal(replacement)
			}
		})
	}
}

func TestDuplicateReadGuardIsBoundedAndExcludesBufferPaging(t *testing.T) {
	var g duplicateReadGuard
	for i := 0; i < maxDuplicateReadEntries+20; i++ {
		call := llm.FunctionCall{Name: "read_file", Arguments: `{}`}
		full := fmt.Sprintf(`{"ok":true,"data":{"path":"file-%d","offset":1,"limit":1}}`, i)
		g.inspect(call, full, "/workspace")
	}
	if len(g.entries) != maxDuplicateReadEntries {
		t.Fatal(len(g.entries))
	}
	for i := 0; i < 5; i++ {
		if replacement, _ := g.inspect(llm.FunctionCall{Name: "read_tool_buffer", Arguments: `{"offset":0}`}, `{"ok":true,"data":[]}`, "/workspace"); replacement != "" {
			t.Fatal(replacement)
		}
	}
}
