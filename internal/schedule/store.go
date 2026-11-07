package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"code-review-agent/internal/config"
)

// Policy is the persisted, human-editable form of the working-hours schedule.
// It exists so /window edits survive a restart instead of living only in memory.
type Policy struct {
	Enabled  bool         `json:"enabled"`
	Timezone string       `json:"timezone"`
	Windows  []WindowSpec `json:"windows"`
	// Channels holds per-channel overrides; an entry with Enabled=false means
	// "this channel ignores the global window" (e.g. GLM runs around the clock).
	Channels map[string]ChannelPolicy `json:"channels,omitempty"`
	SavedAt  string                   `json:"saved_at,omitempty"`
}

type ChannelPolicy struct {
	Enabled  bool         `json:"enabled"`
	Timezone string       `json:"timezone,omitempty"`
	Windows  []WindowSpec `json:"windows"`
}

type WindowSpec struct {
	Start string   `json:"start"`
	End   string   `json:"end"`
	Days  []string `json:"days"`
}

const policyVersion = 1

type policyFile struct {
	Version int    `json:"version"`
	Policy  Policy `json:"policy"`
}

// PolicyOf renders a live schedule back into persisted form.
func (s *Schedule) PolicyOf() Policy {
	if s == nil {
		return Policy{Enabled: false}
	}
	policy := Policy{Enabled: s.Enabled, Timezone: s.Location.String()}
	if !s.Enabled {
		policy.Timezone = "Asia/Shanghai"
	}
	for _, window := range s.Windows {
		policy.Windows = append(policy.Windows, WindowSpec{
			Start: window.ClockStart(), End: window.ClockEnd(), Days: window.DayNames(),
		})
	}
	return policy
}

// NewFromPolicy validates and builds a schedule from persisted editor state.
func NewFromPolicy(policy Policy) (*Schedule, error) {
	cfg := config.ScheduleConfig{Enabled: policy.Enabled, Timezone: policy.Timezone}
	if cfg.Timezone == "" {
		cfg.Timezone = "Asia/Shanghai"
	}
	for _, window := range policy.Windows {
		cfg.Windows = append(cfg.Windows, config.TimeWindowConfig{Start: window.Start, End: window.End, Days: window.Days})
	}
	if !policy.Enabled {
		return &Schedule{}, nil
	}
	if len(cfg.Windows) == 0 {
		return nil, fmt.Errorf("时间段策略已开启但没有任何窗口")
	}
	return New(cfg)
}

// LoadPolicy reads the persisted policy. A missing file is not an error: the
// caller keeps the config-file schedule.
func LoadPolicy(path string) (Policy, bool, error) {
	if path == "" {
		return Policy{}, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Policy{}, false, nil
		}
		return Policy{}, false, err
	}
	var file policyFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Policy{}, false, fmt.Errorf("解析时间段状态失败：%w", err)
	}
	if file.Version != policyVersion {
		return Policy{}, false, fmt.Errorf("不支持的时间段状态版本 %d", file.Version)
	}
	return file.Policy, true, nil
}

// SavePolicy persists editor state atomically.
func SavePolicy(path string, policy Policy) error {
	if path == "" {
		return nil
	}
	policy.SavedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(policyFile{Version: policyVersion, Policy: policy}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".schedule-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// LoadOrDefault prefers the persisted policy, falling back to the config file.
func LoadOrDefault(cfg config.ScheduleConfig) (*Schedule, error) {
	if policy, ok, err := LoadPolicy(cfg.StoreFile); err == nil && ok {
		if schedule, buildErr := NewFromPolicy(policy); buildErr == nil {
			return schedule, nil
		}
	}
	return New(cfg)
}

// ClockStart/ClockEnd expose the window bounds as HH:MM for editing.
func (w Window) ClockStart() string { return clockString(w.StartMinute) }
func (w Window) ClockEnd() string   { return clockString(w.EndMinute) }

func clockString(minutes int) string {
	if minutes < 0 {
		minutes = 0
	}
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// DayNames renders the weekday set in mon..sun order.
func (w Window) DayNames() []string {
	order := []struct {
		day  time.Weekday
		name string
	}{{time.Monday, "mon"}, {time.Tuesday, "tue"}, {time.Wednesday, "wed"}, {time.Thursday, "thu"},
		{time.Friday, "fri"}, {time.Saturday, "sat"}, {time.Sunday, "sun"}}
	var names []string
	for _, entry := range order {
		if w.Days[entry.day] {
			names = append(names, entry.name)
		}
	}
	return names
}

// DescribeDays renders the weekday set compactly for the editor list.
func (w Window) DescribeDays() string {
	names := w.DayNames()
	if len(names) == 0 {
		return "无"
	}
	if len(names) == 7 {
		return "每天"
	}
	index := map[string]int{"mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6, "sun": 7}
	// Contiguous runs of short weekday names collapse to mon-fri style ranges.
	first, last := index[names[0]], index[names[len(names)-1]]
	if len(names) > 2 && last-first == len(names)-1 {
		return names[0] + "-" + names[len(names)-1]
	}
	return strings.Join(names, ",")
}

// ParseDays accepts "all", "weekdays", "weekend", "mon-fri" and comma lists.
func ParseDays(value string) ([]string, error) {
	text := strings.ToLower(strings.TrimSpace(value))
	switch text {
	case "", "all", "每天", "everyday":
		return []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, nil
	case "weekdays", "工作日":
		return []string{"mon", "tue", "wed", "thu", "fri"}, nil
	case "weekend", "周末":
		return []string{"sat", "sun"}, nil
	}
	var days []string
	seen := map[string]bool{}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, startOK := dayIndex(bounds[0])
			end, endOK := dayIndex(bounds[1])
			if !startOK || !endOK {
				return nil, fmt.Errorf("无效的星期范围 %q", part)
			}
			if start > end {
				start, end = end, start
			}
			for i := start; i <= end; i++ {
				name := dayName(i)
				if !seen[name] {
					seen[name] = true
					days = append(days, name)
				}
			}
			continue
		}
		if _, ok := dayIndex(part); !ok {
			return nil, fmt.Errorf("无效的星期 %q", part)
		}
		name := strings.ToLower(part)[:3]
		if !seen[name] {
			seen[name] = true
			days = append(days, name)
		}
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("至少要选一天")
	}
	return days, nil
}

func dayIndex(value string) (int, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 {
		return 0, false
	}
	switch value[:3] {
	case "mon":
		return 1, true
	case "tue":
		return 2, true
	case "wed":
		return 3, true
	case "thu":
		return 4, true
	case "fri":
		return 5, true
	case "sat":
		return 6, true
	case "sun":
		return 7, true
	}
	return 0, false
}

func dayName(index int) string {
	switch index {
	case 1:
		return "mon"
	case 2:
		return "tue"
	case 3:
		return "wed"
	case 4:
		return "thu"
	case 5:
		return "fri"
	case 6:
		return "sat"
	}
	return "sun"
}
