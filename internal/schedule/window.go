// Package schedule implements the "only audit inside these hours" gate.
//
// A window may cross midnight (21:00-08:00) and is anchored to the day it
// starts, which is how people describe night shifts. All comparisons happen in
// one IANA location so DST transitions do not shift the window.
package schedule

import (
	"fmt"
	"strings"
	"time"

	"code-review-agent/internal/config"
)

type Window struct {
	StartMinute int
	EndMinute   int
	Days        map[time.Weekday]bool
	Label       string
}

func (w Window) crossesMidnight() bool { return w.EndMinute <= w.StartMinute }

// Contains reports whether at falls inside this window.
func (w Window) Contains(at time.Time) bool {
	minute := at.Hour()*60 + at.Minute()
	if !w.crossesMidnight() {
		return w.Days[at.Weekday()] && minute >= w.StartMinute && minute < w.EndMinute
	}
	if minute >= w.StartMinute {
		return w.Days[at.Weekday()]
	}
	if minute < w.EndMinute {
		return w.Days[previousDay(at.Weekday())]
	}
	return false
}

// NextOpen returns the first instant strictly after at at which the window
// opens, or zero when it never opens.
func (w Window) NextOpen(at time.Time) time.Time {
	for offset := 0; offset <= 8; offset++ {
		day := at.AddDate(0, 0, offset)
		if !w.Days[day.Weekday()] {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, at.Location()).
			Add(time.Duration(w.StartMinute) * time.Minute)
		if candidate.After(at) {
			return candidate
		}
	}
	return time.Time{}
}

type Schedule struct {
	Enabled  bool
	Location *time.Location
	Windows  []Window
}

func New(cfg config.ScheduleConfig) (*Schedule, error) {
	if !cfg.Enabled {
		return &Schedule{}, nil
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("无效时区 %q: %w", cfg.Timezone, err)
	}
	schedule := &Schedule{Enabled: true, Location: location}
	for i, entry := range cfg.Windows {
		window, err := parseWindow(entry)
		if err != nil {
			return nil, fmt.Errorf("schedule.windows[%d]: %w", i, err)
		}
		schedule.Windows = append(schedule.Windows, window)
	}
	if len(schedule.Windows) == 0 {
		return nil, fmt.Errorf("schedule.enabled 需要至少一个时间窗")
	}
	return schedule, nil
}

func parseWindow(cfg config.TimeWindowConfig) (Window, error) {
	start, ok := parseClock(cfg.Start)
	if !ok {
		return Window{}, fmt.Errorf("start %q 必须是 HH:MM", cfg.Start)
	}
	end, ok := parseClock(cfg.End)
	if !ok {
		return Window{}, fmt.Errorf("end %q 必须是 HH:MM", cfg.End)
	}
	days := map[time.Weekday]bool{}
	if len(cfg.Days) == 0 {
		for day := time.Sunday; day <= time.Saturday; day++ {
			days[day] = true
		}
	}
	for _, name := range cfg.Days {
		day, ok := parseDay(name)
		if !ok {
			return Window{}, fmt.Errorf("无效的星期 %q", name)
		}
		days[day] = true
	}
	return Window{StartMinute: start, EndMinute: end, Days: days, Label: cfg.Start + "-" + cfg.End}, nil
}

// Open reports whether work is allowed at the given instant.
func (s *Schedule) Open(at time.Time) bool {
	if s == nil || !s.Enabled {
		return true
	}
	local := at.In(s.Location)
	for _, window := range s.Windows {
		if window.Contains(local) {
			return true
		}
	}
	return false
}

// NextOpen is the earliest moment any window reopens after at.
func (s *Schedule) NextOpen(at time.Time) time.Time {
	if s == nil || !s.Enabled {
		return time.Time{}
	}
	local := at.In(s.Location)
	var earliest time.Time
	for _, window := range s.Windows {
		candidate := window.NextOpen(local)
		if candidate.IsZero() {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest
}

// Describe renders the configured windows for the status line.
func (s *Schedule) Describe() string {
	if s == nil || !s.Enabled {
		return "不限时段"
	}
	parts := make([]string, 0, len(s.Windows))
	for _, window := range s.Windows {
		parts = append(parts, window.Label)
	}
	return strings.Join(parts, " / ")
}

func previousDay(day time.Weekday) time.Weekday {
	if day == time.Sunday {
		return time.Saturday
	}
	return day - 1
}

func parseClock(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, err := parseDigits(parts[0])
	if err != nil || hour > 24 {
		return 0, false
	}
	minute, err := parseDigits(parts[1])
	if err != nil || minute > 59 {
		return 0, false
	}
	if hour == 24 && minute != 0 {
		return 0, false
	}
	if hour == 24 {
		hour = 23
		minute = 59
	}
	return hour*60 + minute, true
}

func parseDigits(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("empty")
	}
	total := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		total = total*10 + int(r-'0')
	}
	return total, nil
}

func parseDay(value string) (time.Weekday, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 {
		return 0, false
	}
	switch value[:3] {
	case "sun":
		return time.Sunday, true
	case "mon":
		return time.Monday, true
	case "tue":
		return time.Tuesday, true
	case "wed":
		return time.Wednesday, true
	case "thu":
		return time.Thursday, true
	case "fri":
		return time.Friday, true
	case "sat":
		return time.Saturday, true
	}
	return 0, false
}
