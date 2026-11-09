package tools

import (
	"encoding/json"
	"testing"
)

func TestFileReviewAuditSelectorsDoNotExpandScope(t *testing.T) {
	for name, selector := range map[string]string{
		"wildcard": `"pattern":"*"`,
		"patterns": `"patterns":["src/*.java"]`,
		"subdir":   `"dir":"src"`,
		"dirs":     `"dirs":["src"]`,
		"suffix":   `"suffix":".java"`,
		"suffixes": `"suffixes":[".java"]`,
		"root":     `"dir":"./"`,
	} {
		t.Run(name, func(t *testing.T) {
			r := &Registry{
				files:     []FileReview{{Path: "src/Planned.java", Status: "pending"}},
				inventory: []InventoryEntry{{Path: "src/Planned.java"}, {Path: "src/Dependency.java"}},
			}
			r.SetAuditSelectorScope(true)
			result := r.fileReviewUpdate(json.RawMessage(`{` + selector + `,"status":"reviewing"}`))
			if !result.OK || len(r.files) != 1 || r.files[0].Path != "src/Planned.java" || r.files[0].Status != "reviewing" {
				t.Fatalf("audit selector expanded or failed: result=%+v files=%+v", result, r.files)
			}
		})
	}
}

func TestFileReviewAuditSelectorsWithEmptyScopeMatchNothing(t *testing.T) {
	r := &Registry{inventory: []InventoryEntry{{Path: "src/Dependency.java"}}}
	r.SetAuditSelectorScope(true)
	result := r.fileReviewUpdate(json.RawMessage(`{"pattern":"*","status":"reviewing"}`))
	if !result.OK || len(r.files) != 0 {
		t.Fatalf("empty audit scope fell back to inventory: result=%+v files=%+v", result, r.files)
	}
}

func TestFileReviewAuditExplicitPathsCanAddDependencies(t *testing.T) {
	for name, explicit := range map[string]string{
		"path":  `"path":"src/Dependency.java"`,
		"paths": `"paths":["src/Dependency.java"]`,
		"items": `"items":[{"path":"src/Dependency.java","status":"reviewing"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			r := &Registry{files: []FileReview{{Path: "src/Planned.java", Status: "pending"}}}
			r.SetAuditSelectorScope(true)
			result := r.fileReviewUpdate(json.RawMessage(`{` + explicit + `,"status":"reviewing"}`))
			if !result.OK || len(r.files) != 2 || r.files[1].Path != "src/Dependency.java" || r.files[1].Status != "reviewing" {
				t.Fatalf("explicit dependency rejected: result=%+v files=%+v", result, r.files)
			}
			if r.files[0].Status != "pending" {
				t.Fatalf("explicit addition changed unreviewed file: %+v", r.files)
			}
		})
	}
}

func TestFileReviewReconSelectorsCanGrowExistingScope(t *testing.T) {
	r := &Registry{inventory: []InventoryEntry{{Path: "src/First.java"}, {Path: "other/Second.go"}}}
	for _, raw := range []string{
		`{"dir":"src","status":"pending"}`,
		`{"dir":".","status":"pending"}`,
	} {
		if result := r.fileReviewUpdate(json.RawMessage(raw)); !result.OK {
			t.Fatalf("recon selector failed: %+v", result)
		}
	}
	if len(r.files) != 2 {
		t.Fatalf("recon selector failed to expand existing scope: %+v", r.files)
	}
}

func TestFileReviewAuditSelectorScopeSurvivesForkAndRestore(t *testing.T) {
	r := &Registry{inventory: []InventoryEntry{{Path: "src/Planned.java"}, {Path: "src/Dependency.java"}}}
	r.SetAuditSelectorScope(true)
	fork := r.Fork()
	fork.RestoreSnapshot(Snapshot{Files: []FileReview{{Path: "src/Planned.java", Status: "pending"}}})
	result := fork.fileReviewUpdate(json.RawMessage(`{"pattern":"*","status":"reviewing"}`))
	if !result.OK || len(fork.files) != 1 {
		t.Fatalf("fork/restore lost audit selector restriction: result=%+v files=%+v", result, fork.files)
	}
	fork.SetAuditSelectorScope(false)
	if result := fork.fileReviewUpdate(json.RawMessage(`{"suffix":".java","status":"pending"}`)); !result.OK || len(fork.files) != 2 {
		t.Fatalf("switching to recon retained restriction: result=%+v files=%+v", result, fork.files)
	}
}

func TestFileReviewCurrentDirSelectorStaysWithinPlannedFiles(t *testing.T) {
	r := &Registry{
		files: []FileReview{{Path: "src/Important.java", Status: "pending"}},
		inventory: []InventoryEntry{
			{Path: "src/Important.java", Ext: ".java"},
			{Path: "src/Unplanned.java", Ext: ".java"},
		},
	}
	r.SetAuditSelectorScope(true)
	result := r.fileReviewUpdate(json.RawMessage(`{"dir":".","status":"reviewing"}`))
	if !result.OK {
		t.Fatalf("selector update failed: %+v", result)
	}
	if len(r.files) != 1 || r.files[0].Path != "src/Important.java" || r.files[0].Status != "reviewing" {
		t.Fatalf("broad selector escaped planned files: %+v", r.files)
	}
}
