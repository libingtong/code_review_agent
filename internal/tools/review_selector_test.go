package tools

import (
	"encoding/json"
	"testing"
)

func TestFileReviewCurrentDirSelectorStaysWithinPlannedFiles(t *testing.T) {
	r := &Registry{
		files: []FileReview{{Path: "src/Important.java", Status: "pending"}},
		inventory: []InventoryEntry{
			{Path: "src/Important.java", Ext: ".java"},
			{Path: "src/Unplanned.java", Ext: ".java"},
		},
	}
	result := r.fileReviewUpdate(json.RawMessage(`{"dir":".","status":"reviewing"}`))
	if !result.OK {
		t.Fatalf("selector update failed: %+v", result)
	}
	if len(r.files) != 1 || r.files[0].Path != "src/Important.java" || r.files[0].Status != "reviewing" {
		t.Fatalf("broad selector escaped planned files: %+v", r.files)
	}
}
