package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectNoteUpdateBoundsOversizedInput(t *testing.T) {
	r := &Registry{}
	input := strings.Repeat("架构证据\n", maxProjectNoteBytes)
	result := r.projectNoteUpdate(json.RawMessage(`{"note":` + mustJSONString(input) + `}`))
	if !result.OK {
		t.Fatalf("update failed: %+v", result)
	}
	if len(r.projectNote.Note) > maxProjectNoteBytes+128 {
		t.Fatalf("note grew beyond bound: %d", len(r.projectNote.Note))
	}
	if !strings.Contains(result.Message, "bounded") {
		t.Fatalf("result did not report truncation: %q", result.Message)
	}
	if !strings.Contains(r.projectNote.Note, "项目笔记已截断") {
		t.Fatalf("bounded note omitted truncation marker")
	}
}

func TestProjectNoteUpdatePreservesSmallInput(t *testing.T) {
	r := &Registry{}
	const note = "架构：Spring MVC\n结论：继续核验鉴权"
	result := r.projectNoteUpdate(json.RawMessage(`{"note":"架构：Spring MVC\n结论：继续核验鉴权"}`))
	if !result.OK || r.projectNote.Note != note {
		t.Fatalf("small note changed: result=%+v note=%q", result, r.projectNote.Note)
	}
}

func mustJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
