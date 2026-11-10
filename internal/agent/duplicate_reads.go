package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"

	"code-review-agent/internal/llm"
	"code-review-agent/internal/tools"
)

// Keep fingerprints, never file contents. The cap bounds memory for long scans.
const maxDuplicateReadEntries = 512

type duplicateReadEntry struct {
	content        [32]byte
	count          int
	firstCallID    string
	lastEvidenceID string
	recovered      bool
	touched        uint64
}

type duplicateReadGuard struct {
	entries map[[32]byte]duplicateReadEntry
	clock   uint64
}

// inspect checks the actual result, rather than file timestamps: equal-size edits
// and changed directory listings must remain readable. Local reads still verify
// freshness; the third unchanged result is withheld from model context/buffers.
// Failed calls and buffer pagination never consume this success-only allowance.
func (g *duplicateReadGuard) inspect(call llm.FunctionCall, full, workspace string, evidenceVisible ...func(string) bool) (replacement, notice string) {
	switch call.Name {
	case "read_file", "list_files", "search_content", "search_context":
	default:
		return "", ""
	}
	var outcome struct {
		OK      bool            `json:"ok"`
		Data    json.RawMessage `json:"data"`
		Trunc   bool            `json:"truncated"`
		Message string          `json:"message"`
	}
	if json.Unmarshal([]byte(full), &outcome) != nil || !outcome.OK || len(outcome.Data) == 0 {
		return "", ""
	}
	args, err := canonicalReadArguments(call, outcome.Data, workspace)
	if err != nil {
		return "", ""
	}
	var data interface{}
	if json.Unmarshal(outcome.Data, &data) != nil {
		return "", ""
	}
	canonical, _ := json.Marshal(data)
	// Truncation is part of the observed result, not a transient buffer ID.
	fingerprint := sha256.Sum256(append(canonical, byteBool(outcome.Trunc)))
	key := sha256.Sum256(append([]byte(call.Name+"\x00"), args...))
	if g.entries == nil {
		g.entries = make(map[[32]byte]duplicateReadEntry)
	}
	entry, exists := g.entries[key]
	if !exists && len(g.entries) >= maxDuplicateReadEntries {
		var oldest [32]byte
		min := ^uint64(0)
		for k, e := range g.entries {
			if e.touched < min {
				oldest, min = k, e.touched
			}
		}
		delete(g.entries, oldest)
	}
	if !exists || entry.content != fingerprint {
		entry = duplicateReadEntry{content: fingerprint, firstCallID: call.CallID}
	}
	if entry.count < 3 {
		entry.count++
	}
	g.clock++
	entry.touched = g.clock
	// Persist both the bounded counter and delivery metadata, including recovery.
	defer func() { g.entries[key] = entry }()
	if entry.count < 3 {
		entry.lastEvidenceID = call.CallID
	}
	if entry.count < 2 {
		return "", ""
	}
	reference := fmt.Sprintf("同参数且内容未变；已有结果 call_id=%s。请使用已有证据分析并提交结论，或读取新文件/新行范围。", entry.firstCallID)
	if entry.count >= 3 {
		visible := true
		if len(evidenceVisible) > 0 && evidenceVisible[0] != nil {
			visible = evidenceVisible[0](entry.lastEvidenceID)
		}
		if !entry.recovered && entry.lastEvidenceID != "" && !visible {
			// Compression may remove the delivered evidence, but must not grant a fresh
			// repeat allowance on every compression. Recovery is once per fingerprint.
			entry.recovered = true
			entry.lastEvidenceID = call.CallID
			notice = "证据恢复（仅此一次）：先前成功结果已不在当前可见上下文，可能因压缩移除；本次重新提供相同证据，重复计数不清零。" + reference
		} else {
			notice = "重复读取已拦截（第3次及以后）：" + reference
			encoded, _ := json.Marshal(tools.Result{OK: false, Error: notice})
			return string(encoded), notice
		}
	} else {
		notice = "重复读取提醒（第2次）：" + reference + "再次获取相同结果将被拦截。"
	}
	// Put the warning before data so even a bounded preview contains it.
	encoded, _ := json.Marshal(struct {
		OK      bool            `json:"ok"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
		Trunc   bool            `json:"truncated,omitempty"`
	}{true, notice + outcome.Message, outcome.Data, outcome.Trunc})
	return string(encoded), notice
}

func byteBool(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func canonicalReadArguments(call llm.FunctionCall, data json.RawMessage, workspace string) ([]byte, error) {
	if call.Name == "read_file" {
		// The successful result contains resolved paths and applied paging defaults.
		var page struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, err
		}
		return json.Marshal(page)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = make(map[string]interface{})
	}
	// Normalize directory aliases and the defaults used by list_files.
	if call.Name == "list_files" {
		root, _ := args["root"].(string)
		if root == "" {
			root = "."
		}
		if filepath.IsAbs(root) {
			if relative, err := filepath.Rel(workspace, root); err == nil {
				root = relative
			}
		}
		args["root"] = filepath.ToSlash(filepath.Clean(root))
		if n, _ := args["limit"].(float64); n <= 0 {
			args["limit"] = float64(500)
		}
		if _, ok := args["pattern"]; !ok {
			args["pattern"] = ""
		}
		if _, ok := args["max_depth"]; !ok {
			args["max_depth"] = float64(0)
		}
		if _, ok := args["include_hidden"]; !ok {
			args["include_hidden"] = false
		}
	}
	return json.Marshal(args)
}
