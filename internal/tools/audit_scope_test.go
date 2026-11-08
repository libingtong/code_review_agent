package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedWorkspace(t *testing.T, files []string) *Registry {
	t.Helper()
	dir := t.TempDir()
	for _, name := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := NewRegistry(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func TestSuggestAuditFilesRanksSignals(t *testing.T) {
	registry := seedWorkspace(t, []string{
		"src/main/java/app/web/UserController.java",
		"src/main/java/app/web/AuthFilter.java",
		"src/main/java/app/dao/UserDao.java",
		"src/main/java/app/util/StringUtil.java",
		"src/main/java/app/UploadService.java",
		"docs/design.md",
		"src/test/java/app/UserControllerTest.java",
		"README.md",
	})

	got := registry.SuggestAuditFiles(3, 4)
	if len(got) != 3 {
		t.Fatalf("limit not respected: %v", got)
	}
	// Control-plane classes come first; a bare controller is numerous and ranks after
	// them, so it must not eat the first slots.
	want := map[string]bool{
		"src/main/java/app/UploadService.java":  true,
		"src/main/java/app/web/AuthFilter.java": true,
		"src/main/java/app/dao/UserDao.java":    true,
	}
	for _, path := range got {
		if !want[path] {
			t.Fatalf("unexpected first pick: %v", got)
		}
	}
	full := registry.SuggestAuditFiles(10, 4)
	controllerAt := -1
	for index, path := range full {
		if strings.HasSuffix(path, "UserController.java") {
			controllerAt = index
		}
	}
	if controllerAt < 3 {
		t.Fatalf("controller should rank behind the control plane: %v", full)
	}

	all := registry.SuggestAuditFiles(10, 4)
	for _, path := range all {
		switch path {
		case "docs/design.md", "README.md", "src/test/java/app/UserControllerTest.java":
			t.Fatalf("doc/test file must never be seeded: %v", all)
		}
	}
}

func TestSuggestAuditFilesSpreadsAcrossDirectories(t *testing.T) {
	registry := seedWorkspace(t, []string{
		"src/main/java/app/web/AaController.java",
		"src/main/java/app/web/AbController.java",
		"src/main/java/app/web/AcController.java",
		"src/main/java/app/web/AdController.java",
		"src/main/java/app/web/AeController.java",
		"src/main/java/app/api/ApiController.java",
	})

	got := registry.SuggestAuditFiles(6, 2)
	perDir := map[string]int{}
	for _, path := range got {
		perDir[filepath.Dir(path)]++
	}
	// The per-class guarantee may take a couple of extra files from one directory on
	// purpose (the control-plane classes must be represented), but the fill phase
	// still spreads out instead of collapsing into a single package.
	for dir, count := range perDir {
		if count > guaranteedPerBucket+2 {
			t.Fatalf("directory %s took %d slots: %v", dir, count, got)
		}
	}
	if len(got) != 6 {
		t.Fatalf("expected six files, got %v", got)
	}
}

func TestSeedAuditScopeMarksProvenance(t *testing.T) {
	registry := seedWorkspace(t, []string{
		"src/main/java/app/web/UserController.java",
		"src/main/java/app/dao/UserDao.java",
	})

	added := registry.SeedAuditScope([]string{"src/main/java/app/dao/UserDao.java", "missing/File.java"})
	if len(added) != 1 || added[0] != "src/main/java/app/dao/UserDao.java" {
		t.Fatalf("seed must skip unknown paths: %v", added)
	}
	snapshot := registry.Snapshot()
	if len(snapshot.Files) != 1 || snapshot.Files[0].Note != "系统按风险关键词补入审计范围" {
		t.Fatalf("seed provenance missing: %+v", snapshot.Files)
	}
	if snapshot.Files[0].Status != "reviewing" {
		t.Fatalf("seeded file must be part of the scope: %+v", snapshot.Files[0])
	}
}

// Coverage used to sit at 0/48 while agents had already read 41 files, because
// claiming was a separate optional call. Reading must move the number.
func TestReadFileMarksScopedAndIncidentalFilesReviewing(t *testing.T) {
	registry := seedWorkspace(t, []string{
		"src/main/java/app/web/UserController.java",
		"src/main/java/app/web/OrderController.java",
	})
	registry.PlanAuditScope([]string{"src/main/java/app/web/UserController.java"})

	for _, path := range []string{"src/main/java/app/web/UserController.java", "src/main/java/app/web/OrderController.java"} {
		out := registry.Call("read_file", json.RawMessage(`{"path":"`+path+`","limit":5}`))
		if !strings.Contains(out, `"ok": true`) {
			t.Fatalf("read %s failed: %s", path, out)
		}
	}
	statuses := map[string]string{}
	for _, file := range registry.Snapshot().Files {
		statuses[file.Path] = file.Status
	}
	for _, want := range []string{"src/main/java/app/web/UserController.java", "src/main/java/app/web/OrderController.java"} {
		if statuses[want] != "reviewing" {
			t.Fatalf("read file not marked reviewing: %v", statuses)
		}
	}
	coverage := ComputeCoverage(registry.Snapshot())
	if coverage.Total != 2 || coverage.Reviewing != 2 {
		t.Fatalf("coverage = %+v", coverage)
	}
}
