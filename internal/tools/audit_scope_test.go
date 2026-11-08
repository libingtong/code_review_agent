package tools

import (
	"os"
	"path/filepath"
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
	want := map[string]bool{
		"src/main/java/app/web/UserController.java": true,
		"src/main/java/app/web/AuthFilter.java":     true,
		"src/main/java/app/UploadService.java":      true,
	}
	for _, path := range got {
		if !want[path] {
			t.Fatalf("low-signal file ranked too high: %v", got)
		}
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
	for dir, count := range perDir {
		if count > 2 {
			t.Fatalf("directory %s took %d slots: %v", dir, count, got)
		}
	}
	// One file in .../api plus two from .../web: the per-directory cap keeps the
	// quota from collapsing into a single package.
	if len(got) != 3 {
		t.Fatalf("expected the per-directory cap to apply, got %v", got)
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
