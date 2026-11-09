package tools

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const maxProjectNoteBytes = 64 * 1024

const projectNoteTruncationMarker = "\n\n[项目笔记已截断：保留开头与最新结论，原始输入超过 64KiB]\n\n"

type projectNoteUpdateArgs struct {
	Note string `json:"note"`
}

func boundProjectNote(note string) (string, bool) {
	if len(note) <= maxProjectNoteBytes {
		return note, false
	}
	// Keep the architecture/identity prefix and the newest conclusions at the end.
	// Cut on rune boundaries so malformed UTF-8 never enters the shared state.
	budget := maxProjectNoteBytes - len(projectNoteTruncationMarker)
	head := budget * 3 / 4
	tail := budget - head
	for head > 0 && !utf8.RuneStart(note[head]) {
		head--
	}
	start := len(note) - tail
	for start < len(note) && !utf8.RuneStart(note[start]) {
		start++
	}
	return note[:head] + projectNoteTruncationMarker + note[start:], true
}

func BoundProjectNote(note string) string {
	bounded, _ := boundProjectNote(note)
	return bounded
}

func (r *Registry) projectNoteUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[projectNoteUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Note == "" {
		return Result{OK: false, Error: "note is required"}
	}
	bounded, truncated := boundProjectNote(args.Note)
	r.projectNote.Note = bounded
	if truncated {
		return Result{OK: true, Data: r.projectNote, Message: fmt.Sprintf("project note updated and bounded to %d bytes", len(bounded))}
	}
	return Result{OK: true, Data: r.projectNote, Message: "project note updated"}
}

func formatProjectNote(note ProjectNote) string {
	if note.Note == "" {
		return "暂无项目笔记。规划阶段必须调用 project_note_update 维护详细自由文本笔记，像人工审计员一样记录项目架构、行为、登录认证、鉴权、攻击面、数据/状态流、关键文件角色、已知结论和待确认问题。\n"
	}
	return note.Note + "\n"
}
