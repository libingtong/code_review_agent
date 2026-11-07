package applog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"code-review-agent/internal/config"
)

func testConfig(dir string) config.LoggingConfig {
	enabled := true
	_ = enabled
	return config.LoggingConfig{Enabled: true, Level: "debug", Dir: dir, MaxSizeMB: 1, KeepFiles: 2, LLMDump: true}
}

func TestLoggerRespectsLevelAndRedacts(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	logger.Debugf("debug %s", "sk-abcdefghijklmnopqrstuvwxyz")
	logger.SetLevel(LevelWarn)
	logger.Infof("info should be filtered")
	logger.Warnf("warn %s", "Bearer abcdefghijklmnopqrstuvwxyz")

	data, err := os.ReadFile(logger.Path())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "DEBUG") {
		t.Fatalf("debug line missing: %s", text)
	}
	if strings.Contains(text, "info should be filtered") {
		t.Fatalf("level filter ignored: %s", text)
	}
	if !strings.Contains(text, "WARN") {
		t.Fatalf("warn line missing: %s", text)
	}
	if strings.Contains(text, "sk-abcdefghijklmnopqrstuvwxyz") || strings.Contains(text, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("secret leaked into log: %s", text)
	}
}

func TestDumpJSONRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	path, err := logger.DumpJSON("llm-responses", map[string]any{
		"authorization": "Bearer sk-secretsecretsecret",
		"note":          "key sk-abcdefghijklmnop",
		"nested":        map[string]any{"api_key": "sk-zzzzzzzzzzzzzzzz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, needle := range []string{"sk-secretsecretsecret", "sk-abcdefghijklmnop", "sk-zzzzzzzzzzzzzzzz"} {
		if strings.Contains(text, needle) {
			t.Fatalf("dump leaked %q: %s", needle, text)
		}
	}
	if !strings.Contains(text, "***") {
		t.Fatalf("dump should mask secrets: %s", text)
	}
	if filepath.Dir(path) != filepath.Join(dir, "dumps") {
		t.Fatalf("dump location = %s", path)
	}
}

func TestDisabledLoggerIsNoop(t *testing.T) {
	logger, err := Open(config.LoggingConfig{Enabled: false})
	if err != nil || logger != nil {
		t.Fatalf("disabled logging must be nil: %v %v", logger, err)
	}
	// A nil logger must be safe to use everywhere.
	logger.Infof("noop")
	if logger.Path() != "" || logger.LLMDump() {
		t.Fatal("nil logger should report empty state")
	}
}

// Concurrent failures must never share a dump file: interleaved writes corrupt it.
func TestDumpJSONIsSafeUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	logger, err := Open(testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	const n = 24
	var wg sync.WaitGroup
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path, err := logger.DumpJSON("llm-responses-tools", map[string]any{"i": i})
			if err != nil {
				t.Errorf("dump %d: %v", i, err)
				return
			}
			paths[i] = path
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			t.Fatal("missing dump path")
		}
		if seen[path] {
			t.Fatalf("dump path reused: %s", path)
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("corrupt dump %s: %v", path, err)
		}
	}
}
