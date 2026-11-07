package schedule

import (
	"sync"
	"time"

	"code-review-agent/internal/config"
)

// Plan is the two-level working-hours policy: one global window plus optional
// per-channel overrides. An override replaces the global policy for that channel
// (including "disabled = always usable"), which is how GLM can run around the
// clock while MiniMax stays restricted to the night.
type Plan struct {
	mu       sync.RWMutex
	global   *Schedule
	channels map[string]*Schedule
	order    []string
}

func NewPlan(global *Schedule) *Plan {
	if global == nil {
		global = &Schedule{}
	}
	return &Plan{global: global, channels: map[string]*Schedule{}}
}

// FromConfig builds the plan from the global schedule section plus each
// provider's optional override.
func FromConfig(cfg config.Config) (*Plan, error) {
	global, err := LoadOrDefault(cfg.Schedule)
	if err != nil {
		return nil, err
	}
	plan := NewPlan(global)
	for _, provider := range cfg.Providers {
		plan.order = append(plan.order, provider.ID)
		if provider.Schedule == nil {
			continue
		}
		channel, err := New(*provider.Schedule)
		if err != nil {
			return nil, err
		}
		plan.channels[provider.ID] = channel
	}
	return plan, nil
}

// SetGlobal replaces the global policy.
func (p *Plan) SetGlobal(s *Schedule) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.global = s
	p.mu.Unlock()
}

// Global returns the global policy.
func (p *Plan) Global() *Schedule {
	if p == nil {
		return &Schedule{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.global
}

// SetChannel installs (or clears, with nil) an override for one channel.
func (p *Plan) SetChannel(id string, s *Schedule) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.channels == nil {
		p.channels = map[string]*Schedule{}
	}
	if s == nil {
		delete(p.channels, id)
		return
	}
	if _, exists := p.channels[id]; !exists {
		p.order = append(p.order, id)
	}
	p.channels[id] = s
}

// Channel returns the effective policy for one channel (override or global).
func (p *Plan) Channel(id string) *Schedule {
	if p == nil {
		return &Schedule{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if s, ok := p.channels[id]; ok {
		return s
	}
	return p.global
}

// Overridden reports whether the channel has its own policy.
func (p *Plan) Overridden(id string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.channels[id]
	return ok
}

// Channels lists configured channel ids, for status output.
func (p *Plan) Channels() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.order...)
}

// OpenAt implements the pool's channel clock: only sources whose own policy is
// open right now may receive work.
func (p *Plan) OpenAt(channel string, at time.Time) bool {
	return p.Channel(channel).Open(at)
}

// NextOpenAt is when a given channel becomes usable again.
func (p *Plan) NextOpenAt(channel string, at time.Time) time.Time {
	return p.Channel(channel).NextOpen(at)
}

// AnyOpen reports whether at least one channel may run now.
func (p *Plan) AnyOpen(at time.Time) bool {
	if p == nil {
		return true
	}
	if p.Global().Open(at) {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, channel := range p.channels {
		if channel.Open(at) {
			return true
		}
	}
	return false
}

// NextOpen is the earliest moment any channel reopens.
func (p *Plan) NextOpen(at time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	earliest := p.Global().NextOpen(at)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, channel := range p.channels {
		candidate := channel.NextOpen(at)
		if candidate.IsZero() {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest
}

// PolicyOf renders the whole plan (global + overrides) for persistence.
func (p *Plan) PolicyOf() Policy {
	if p == nil {
		return Policy{}
	}
	policy := p.Global().PolicyOf()
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.channels) > 0 {
		policy.Channels = map[string]ChannelPolicy{}
		for id, channel := range p.channels {
			rendered := channel.PolicyOf()
			policy.Channels[id] = ChannelPolicy{Enabled: rendered.Enabled, Windows: rendered.Windows,
				Timezone: rendered.Timezone}
		}
	}
	return policy
}

// ApplyPolicy replaces the whole plan from persisted state.
func (p *Plan) ApplyPolicy(policy Policy) error {
	global, err := NewFromPolicy(policy)
	if err != nil {
		return err
	}
	p.SetGlobal(global)
	for id, channel := range policy.Channels {
		if !channel.Enabled {
			// Disabled override = always usable for this channel.
			p.SetChannel(id, &Schedule{})
			continue
		}
		built, buildErr := NewFromPolicy(Policy{Enabled: true, Timezone: channel.Timezone, Windows: channel.Windows})
		if buildErr != nil {
			return buildErr
		}
		p.SetChannel(id, built)
	}
	return nil
}
