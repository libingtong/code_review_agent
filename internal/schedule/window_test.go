package schedule

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/provider"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func nightly(t *testing.T) *Schedule {
	t.Helper()
	schedule, err := New(config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []config.TimeWindowConfig{{Start: "21:00", End: "08:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	return schedule
}

func TestNightWindowCrossesMidnight(t *testing.T) {
	schedule := nightly(t)
	loc := shanghai(t)
	cases := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 11, 6, 20, 59, 0, 0, loc), false},
		{time.Date(2026, 11, 6, 21, 0, 0, 0, loc), true},
		{time.Date(2026, 11, 6, 23, 30, 0, 0, loc), true},
		{time.Date(2026, 11, 7, 0, 30, 0, 0, loc), true},
		{time.Date(2026, 11, 7, 7, 59, 0, 0, loc), true},
		{time.Date(2026, 11, 7, 8, 0, 0, 0, loc), false},
		{time.Date(2026, 11, 7, 12, 0, 0, 0, loc), false},
	}
	for _, test := range cases {
		if got := schedule.Open(test.at); got != test.want {
			t.Fatalf("%s: open=%v want %v", test.at, got, test.want)
		}
	}
}

func TestWeekdayRestrictionAnchorsToStartDay(t *testing.T) {
	schedule, err := New(config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []config.TimeWindowConfig{{Start: "22:00", End: "06:00", Days: []string{"fri"}}}})
	if err != nil {
		t.Fatal(err)
	}
	loc := shanghai(t)
	// 2026-11-06 is a Friday: the window runs Fri 22:00 -> Sat 06:00.
	if !schedule.Open(time.Date(2026, 11, 6, 23, 0, 0, 0, loc)) {
		t.Fatal("Friday night must be open")
	}
	if !schedule.Open(time.Date(2026, 11, 7, 5, 0, 0, 0, loc)) {
		t.Fatal("Saturday early morning belongs to Friday's window")
	}
	if schedule.Open(time.Date(2026, 11, 7, 23, 0, 0, 0, loc)) {
		t.Fatal("Saturday night must be closed: only Friday starts a window")
	}
}

func TestNextOpenFindsTheNextEvening(t *testing.T) {
	schedule := nightly(t)
	loc := shanghai(t)
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, loc)
	next := schedule.NextOpen(at)
	want := time.Date(2026, 11, 7, 21, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("next open = %s, want %s", next, want)
	}
	// Already inside the window: the next opening is tomorrow evening.
	inside := time.Date(2026, 11, 7, 22, 0, 0, 0, loc)
	if got := schedule.NextOpen(inside); !got.Equal(time.Date(2026, 11, 8, 21, 0, 0, 0, loc)) {
		t.Fatalf("next open from inside = %s", got)
	}
}

func TestDisabledScheduleIsAlwaysOpen(t *testing.T) {
	schedule, err := New(config.ScheduleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.Open(time.Now()) || !schedule.NextOpen(time.Now()).IsZero() {
		t.Fatal("a disabled schedule must never gate work")
	}
	if schedule.Describe() != "不限时段" {
		t.Fatalf("describe = %q", schedule.Describe())
	}
}

func TestDSTDayKeepsWallClockWindow(t *testing.T) {
	// New York springs forward on 2026-03-08 at 02:00.
	schedule, err := New(config.ScheduleConfig{Enabled: true, Timezone: "America/New_York",
		Windows: []config.TimeWindowConfig{{Start: "21:00", End: "08:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.Open(time.Date(2026, 3, 8, 21, 30, 0, 0, loc)) {
		t.Fatal("window must stay anchored to local wall clock across DST")
	}
	if schedule.Open(time.Date(2026, 3, 8, 12, 0, 0, 0, loc)) {
		t.Fatal("midday must stay closed")
	}
}

type fixedQuota struct {
	budget provider.QuotaBudget
}

func (f fixedQuota) EarliestReset() provider.QuotaBudget { return f.budget }

func TestGatePrefersWindowThenQuota(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 11, 7, 23, 0, 0, 0, loc)
	closed := &Schedule{Enabled: true, Location: loc,
		Windows: []Window{{StartMinute: 9 * 60, EndMinute: 17 * 60, Days: allDays(), Label: "09:00-17:00"}}}
	gate := &Gate{Schedule: closed, Now: func() time.Time { return now },
		Quota: fixedQuota{budget: provider.QuotaBudget{EarliestReset: now.Add(time.Hour), Reason: "额度耗尽"}}}
	decision := gate.Check()
	if decision.Kind != PauseWindow {
		t.Fatalf("window policy must win over quota state: %+v", decision)
	}

	open := nightly(t)
	gate = &Gate{Schedule: open, Now: func() time.Time { return now },
		Quota: fixedQuota{budget: provider.QuotaBudget{EarliestReset: now.Add(time.Hour), Reason: "额度耗尽"}}}
	decision = gate.Check()
	if decision.Kind != PauseQuota || decision.Allowed || decision.ResumeAt.IsZero() {
		t.Fatalf("quota pause not reported: %+v", decision)
	}

	gate.Quota = fixedQuota{}
	if decision := gate.Check(); !decision.Allowed || decision.Kind != Allow {
		t.Fatalf("both gates open should allow work: %+v", decision)
	}
}

func allDays() map[time.Weekday]bool {
	days := map[time.Weekday]bool{}
	for day := time.Sunday; day <= time.Saturday; day++ {
		days[day] = true
	}
	return days
}

func TestParseDaysAcceptsRangesAndPresets(t *testing.T) {
	cases := map[string][]string{
		"all":         {"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
		"weekdays":    {"mon", "tue", "wed", "thu", "fri"},
		"weekend":     {"sat", "sun"},
		"mon-fri":     {"mon", "tue", "wed", "thu", "fri"},
		"fri-mon":     {"mon", "tue", "wed", "thu", "fri"},
		"mon,wed,fri": {"mon", "wed", "fri"},
		"周五":          nil, // invalid
	}
	for input, want := range cases {
		got, err := ParseDays(input)
		if want == nil {
			if err == nil {
				t.Fatalf("%q: expected an error", input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%q: got %v want %v", input, got, want)
		}
	}
	if _, err := ParseDays(""); err != nil {
		t.Fatalf("empty should mean every day: %v", err)
	}
}

func TestDescribeDaysCollapsesRuns(t *testing.T) {
	policy := Policy{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []WindowSpec{{Start: "22:00", End: "06:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}}}}
	parsed, err := NewFromPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Windows[0].DescribeDays(); got != "mon-fri" {
		t.Fatalf("describe = %q", got)
	}
	if got := parsed.Windows[0].ClockStart(); got != "22:00" {
		t.Fatalf("start = %q", got)
	}
	round := parsed.PolicyOf()
	if len(round.Windows) != 1 || strings.Join(round.Windows[0].Days, ",") != "mon,tue,wed,thu,fri" {
		t.Fatalf("policy round trip lost days: %+v", round)
	}
}

func TestPolicyPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedule.json")
	live, err := New(config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []config.TimeWindowConfig{{Start: "21:00", End: "08:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePolicy(path, live.PolicyOf()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrDefault(config.ScheduleConfig{StoreFile: path, Timezone: "UTC",
		Windows: []config.TimeWindowConfig{{Start: "09:00", End: "17:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Describe() != "21:00-08:00" {
		t.Fatalf("persisted policy must win over the config file, got %s", reloaded.Describe())
	}
}

func TestPolicyFallsBackToConfigWhenMissing(t *testing.T) {
	cfg := config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai", StoreFile: filepath.Join(t.TempDir(), "absent.json"),
		Windows: []config.TimeWindowConfig{{Start: "09:00", End: "17:00"}}}
	schedule, err := LoadOrDefault(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Describe() != "09:00-17:00" {
		t.Fatalf("fallback schedule = %s", schedule.Describe())
	}
}

func TestDisabledPolicyHasNoWindows(t *testing.T) {
	schedule, err := NewFromPolicy(Policy{Enabled: false, Windows: []WindowSpec{{Start: "21:00", End: "08:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Enabled || len(schedule.Windows) != 0 {
		t.Fatal("a disabled policy must not carry windows")
	}
	if !schedule.Open(time.Now()) {
		t.Fatal("a disabled policy must allow work")
	}
}

func TestPlanChannelOverrideBeatsGlobal(t *testing.T) {
	loc := shanghai(t)
	global, err := New(config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []config.TimeWindowConfig{{Start: "21:00", End: "08:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	plan := NewPlan(global)
	// GLM ignores the global window: `enabled: false` means always usable.
	plan.SetChannel("glm", &Schedule{})

	night := time.Date(2026, 11, 7, 23, 0, 0, 0, loc)
	day := time.Date(2026, 11, 7, 10, 0, 0, 0, loc)

	if !plan.OpenAt("glm", day) {
		t.Fatal("overridden channel must be usable during the day")
	}
	if plan.OpenAt("minimax", day) {
		t.Fatal("channel without override must follow the global window")
	}
	if !plan.OpenAt("minimax", night) || !plan.OpenAt("glm", night) {
		t.Fatal("both channels must be usable at night")
	}
	if !plan.AnyOpen(day) {
		t.Fatal("one open channel is enough to let the program run")
	}
	if !plan.Overridden("glm") || plan.Overridden("minimax") {
		t.Fatal("override bookkeeping wrong")
	}
}

func TestPlanChannelOwnWindow(t *testing.T) {
	loc := shanghai(t)
	plan := NewPlan(&Schedule{Enabled: true, Location: loc,
		Windows: []Window{{StartMinute: 21 * 60, EndMinute: 8 * 60, Days: allDays(), Label: "21:00-08:00"}}})
	channel, err := New(config.ScheduleConfig{Enabled: true, Timezone: "Asia/Shanghai",
		Windows: []config.TimeWindowConfig{{Start: "09:00", End: "18:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}}}})
	if err != nil {
		t.Fatal(err)
	}
	plan.SetChannel("day-channel", channel)

	// Monday 10:00 is inside the channel window but outside the global one.
	monday := time.Date(2026, 11, 9, 10, 0, 0, 0, loc)
	if !plan.OpenAt("day-channel", monday) {
		t.Fatal("channel window must apply")
	}
	if plan.OpenAt("other", monday) {
		t.Fatal("channel without window must stay closed")
	}
	if got := plan.NextOpenAt("other", monday); !got.Equal(time.Date(2026, 11, 9, 21, 0, 0, 0, loc)) {
		t.Fatalf("next open for the global channel = %s", got)
	}
}

func TestPlanPolicyRoundTripWithChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedule.json")
	global := &Schedule{Enabled: true, Location: time.UTC,
		Windows: []Window{{StartMinute: 21 * 60, EndMinute: 8 * 60, Days: allDays(), Label: "21:00-08:00"}}}
	plan := NewPlan(global)
	plan.SetChannel("glm", &Schedule{}) // always
	if err := SavePolicy(path, plan.PolicyOf()); err != nil {
		t.Fatal(err)
	}
	policy, ok, err := LoadPolicy(path)
	if err != nil || !ok {
		t.Fatalf("policy not saved: %v %v", err, ok)
	}
	restored := NewPlan(&Schedule{})
	if err := restored.ApplyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if !restored.Overridden("glm") {
		t.Fatal("channel override lost in round trip")
	}
	if !restored.OpenAt("glm", time.Now()) {
		t.Fatal("restored always-channel must be open")
	}
	if restored.OpenAt("minimax", time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)) {
		t.Fatal("restored global window not applied")
	}
}
