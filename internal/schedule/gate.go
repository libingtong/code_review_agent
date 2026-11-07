package schedule

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/provider"
)

// QuotaWatch is the quota view the gate needs. It is an interface so scheduling
// can be tested without a provider pool.
type QuotaWatch interface {
	EarliestReset() provider.QuotaBudget
}

type DecisionKind string

const (
	Allow       DecisionKind = "allow"
	PauseWindow DecisionKind = "window_closed"
	PauseQuota  DecisionKind = "quota_exhausted"
)

// Decision is the admission verdict for one instant.
type Decision struct {
	Kind     DecisionKind
	Allowed  bool
	Reason   string
	ResumeAt time.Time
	// Detail explains each channel's individual brake when quota is the cause.
	Detail string
}

// Gate combines the working-hours window with provider quota availability.
// The two pauses are deliberately distinguishable: one is a policy the user set,
// the other is a vendor limit that clears on its own.
type Gate struct {
	mu       sync.RWMutex
	policy   *Plan
	Schedule *Schedule // optional shortcut for the global policy (tests/embedding)
	Quota    QuotaWatch
	Now      func() time.Time
}

// SetSchedule swaps the global policy at runtime.
func (g *Gate) SetSchedule(s *Schedule) { g.Plan().SetGlobal(s) }

// CurrentSchedule returns the global policy for display/editing.
func (g *Gate) CurrentSchedule() *Schedule { return g.Plan().Global() }

// Plan returns the two-level policy, creating one if absent. A bare Schedule
// field (used by older callers and tests) seeds the global policy.
func (g *Gate) Plan() *Plan {
	if g == nil {
		return NewPlan(&Schedule{})
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.policy == nil {
		global := g.Schedule
		if global == nil {
			global = &Schedule{}
		}
		g.policy = NewPlan(global)
	}
	return g.policy
}

// SetPlan installs a fully built policy.
func (g *Gate) SetPlan(plan *Plan) {
	g.mu.Lock()
	g.policy = plan
	if plan != nil {
		g.Schedule = plan.Global()
	}
	g.mu.Unlock()
}

func (g *Gate) now() time.Time {
	if g != nil && g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Check decides whether work may start right now.
func (g *Gate) Check() Decision {
	now := g.now()
	if g != nil {
		plan := g.Plan()
		if !plan.AnyOpen(now) {
			return Decision{Kind: PauseWindow, Reason: "所有渠道都不在工作时段（" + g.Describe() + "）", ResumeAt: plan.NextOpen(now)}
		}
	}
	if g != nil && g.Quota != nil {
		budget := g.Quota.EarliestReset()
		if !budget.EarliestReset.IsZero() && budget.EarliestReset.After(now) {
			return Decision{Kind: PauseQuota, Reason: budget.Reason, ResumeAt: budget.EarliestReset, Detail: budget.Detail}
		}
	}
	return Decision{Kind: Allow, Allowed: true}
}

// Describe renders the gate state for the status line.
func (g *Gate) Describe() string {
	if g == nil {
		return "不限时段"
	}
	plan := g.Plan()
	now := g.now()
	global := plan.Global()
	text := global.Describe()
	if len(plan.Channels()) == 0 {
		state := "关闭"
		if global.Open(now) {
			state = "开启"
		}
		return text + " " + state
	}
	parts := []string{text}
	for _, id := range plan.Channels() {
		channel := plan.Channel(id)
		state := "开"
		if !channel.Open(now) {
			state = "关"
		}
		label := channel.Describe()
		if !plan.Overridden(id) {
			label = "全局"
		}
		parts = append(parts, fmt.Sprintf("%s=%s%s", id, label, state))
	}
	return strings.Join(parts, " · ")
}
