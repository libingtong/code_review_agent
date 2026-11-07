// Package applog is the diagnostic log: opt-in, level-filtered, file-tailored
// and secret-redacting. It exists because vendor protocol differences can only
// be understood by looking at the actual request/response bytes.
package applog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/config"
)

type Level int

const (
	LevelOff Level = iota
	LevelError
	LevelWarn
	LevelInfo
	LevelDebug
)

func ParseLevel(value string) Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "none", "false":
		return LevelOff
	case "error":
		return LevelError
	case "warn", "warning":
		return LevelWarn
	case "debug", "trace":
		return LevelDebug
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelError:
		return "ERROR"
	case LevelWarn:
		return "WARN"
	case LevelInfo:
		return "INFO"
	case LevelDebug:
		return "DEBUG"
	}
	return "OFF"
}

// Logger is safe for concurrent use; a nil *Logger is a no-op logger so call
// sites never need nil checks.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	dir     string
	level   Level
	maxSize int64
	keep    int
	dump    bool
	size    int64
}

var (
	globalMu sync.RWMutex
	global   *Logger
)

// L returns the process logger (never nil).
func L() *Logger {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

// SetLogger installs the process logger (nil disables logging).
func SetLogger(logger *Logger) {
	globalMu.Lock()
	global = logger
	globalMu.Unlock()
}

// Open starts a logger according to cfg. A disabled config returns nil.
func Open(cfg config.LoggingConfig) (*Logger, error) {
	level := ParseLevel(cfg.Level)
	if !cfg.Enabled || level == LevelOff {
		return nil, nil
	}
	dir := cfg.Dir
	if dir == "" {
		dir = "logs"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	logger := &Logger{dir: dir, level: level, maxSize: int64(cfg.MaxSizeMB) * 1024 * 1024, keep: cfg.KeepFiles, dump: cfg.LLMDump}
	if logger.maxSize <= 0 {
		logger.maxSize = 32 * 1024 * 1024
	}
	if logger.keep <= 0 {
		logger.keep = 5
	}
	if err := logger.openFile(); err != nil {
		return nil, err
	}
	return logger, nil
}

func (l *Logger) openFile() error {
	name := filepath.Join(l.dir, "app-"+time.Now().Format("20060102-150405")+".log")
	file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	l.file, l.path, l.size = file, name, info.Size()
	return nil
}

// Path is the current log file ("" when logging is off).
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Level reports the active threshold.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelOff
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// SetLevel changes the threshold at runtime (/log debug).
func (l *Logger) SetLevel(level Level) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

// LLMDump reports whether failing model requests are dumped verbatim.
func (l *Logger) LLMDump() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dump
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *Logger) logf(level Level, format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil || level > l.level {
		return
	}
	line := fmt.Sprintf("%s %-5s %s\n", time.Now().Format("2006-01-02T15:04:05.000-07:00"), level.String(), Redact(fmt.Sprintf(format, args...)))
	n, err := l.file.WriteString(line)
	if err != nil {
		return
	}
	l.size += int64(n)
	if l.size >= l.maxSize {
		l.rotateLocked()
	}
}

// rotateLocked renames the current file to .1 and starts a fresh one.
func (l *Logger) rotateLocked() {
	if l.file == nil {
		return
	}
	l.file.Close()
	l.file = nil
	base := strings.TrimSuffix(l.path, ".log")
	for i := l.keep; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d.log", base, i)
		to := fmt.Sprintf("%s.%d.log", base, i+1)
		if i == l.keep {
			_ = os.Remove(from)
			continue
		}
		_ = os.Rename(from, to)
	}
	_ = os.Rename(l.path, base+".1.log")
	_ = l.openFile()
}

func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }

// DumpJSON writes a redacted JSON artifact (model request/response, session
// snapshot) into <dir>/dumps for offline inspection.
func (l *Logger) DumpJSON(name string, payload any) (string, error) {
	if l == nil || !l.LLMDump() {
		return "", nil
	}
	l.mu.Lock()
	dir := l.dir
	l.mu.Unlock()
	dumpDir := filepath.Join(dir, "dumps")
	if err := os.MkdirAll(dumpDir, 0o700); err != nil {
		return "", err
	}
	sanitized := RedactValue(payload)
	data, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		return "", err
	}
	safe := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "-")
	path := filepath.Join(dumpDir, time.Now().Format("20060102-150405.000")+"-"+safe+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	l.Infof("dump written: %s", path)
	return path, nil
}

// secretPatterns cover the shapes these APIs actually use.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{12,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),
	regexp.MustCompile(`sk-cp-[A-Za-z0-9_\-]{12,}`),
}

// Redact masks credentials in free-form text.
func Redact(text string) string {
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllStringFunc(text, func(match string) string {
			if len(match) <= 8 {
				return "***"
			}
			return match[:4] + "***" + match[len(match)-2:]
		})
	}
	return text
}

// RedactValue walks a decoded JSON-like value and masks secret-looking keys.
// Payload dumps go through here before hitting disk.
func RedactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") ||
				strings.Contains(lower, "apikey") || strings.Contains(lower, "token") && strings.Contains(lower, "key") {
				out[key] = "***"
				continue
			}
			out[key] = RedactValue(entry)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") {
				out[key] = "***"
				continue
			}
			out[key] = Redact(entry)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = RedactValue(entry)
		}
		return out
	case string:
		return Redact(typed)
	default:
		return value
	}
}


