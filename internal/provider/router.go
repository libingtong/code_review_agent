package provider

import (
	"context"
	"fmt"
	"time"

	"code-review-agent/internal/llm"
)

// maxLineageEntries bounds the continuation ownership map. Agent conversations
// are long, so this is a cache, not a ledger: eviction only costs one extra
// full-history request.
const maxLineageEntries = 2048

// runRequest executes one logical request with failover and adaptive
// concurrency. It is the single place where a provider is chosen, throttled,
// charged and swapped, keeping the agent layer free of vendor knowledge.
//
// 429 handling is deliberately different from quota exhaustion: a rate limit
// usually means "too many parallel requests for this plan", so the pool lowers
// that source's concurrency and keeps going instead of pausing the team.
func runRequest[T any](p *Pool, ctx context.Context, label string, emitted *bool, call func(context.Context, *llm.OpenAIClient) (T, error)) (T, string, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}
	excluded := map[string]bool{}
	var lastErr error
	throttled := 0
	for {
		if err := ctx.Err(); err != nil {
			return zero, "", err
		}
		provider := p.nextProvider(excluded)
		if provider == nil {
			if lastErr == nil {
				return zero, "", ErrAllExhausted
			}
			if !p.hasReadyProvider() {
				return zero, "", fmt.Errorf("%w（最近错误：%v）", ErrAllExhausted, lastErr)
			}
			return zero, "", lastErr
		}
		requestCtx := p.observe(ctx, provider.ID())
		requestCtx = p.affinityContext(requestCtx, provider.ID())
		client := provider.client
		if label == "chat" && provider.aux != nil {
			client = provider.aux
		}

		release, acquireErr := provider.limiter.Acquire(ctx)
		if acquireErr != nil {
			return zero, provider.ID(), acquireErr
		}
		result, err := call(requestCtx, client)
		release()
		if err == nil {
			p.markSuccess(provider.ID())
			if changed, limit := provider.limiter.OnSuccess(); changed {
				p.notifyConcurrency(provider.ID(), limit-1, limit, "连续成功，试探更高并发")
			}
			return result, provider.ID(), nil
		}
		if ctx.Err() != nil {
			return zero, provider.ID(), ctx.Err()
		}
		kind := classifyFailure(err)
		p.recordFailure(provider.ID(), kind, err)

		if kind == FailureRateLimited {
			throttled++
			before, after := provider.limiter.OnRateLimited()
			if after != before {
				p.notifyConcurrency(provider.ID(), before, after, "429 限流，降低并发")
			}
			// Another ready source is a better answer than waiting: its plan has
			// separate concurrency budget.
			if next := p.nextProvider(excluded); next != nil && next.ID() != provider.ID() {
				p.notifySwitch(provider.ID(), next.ID(), "429 限流，改用其它源")
				continue
			}
			// The hard cap is a safety net: even if the limiter has not reached its
			// configured minimum, repeated 429s mean the plan, not parallelism.
			if (provider.limiter.AtMinimum() && throttled >= 3) || throttled >= 8 {
				// Concurrency is already minimal and 429 persists: this is plan
				// quota, not parallelism. Park the source and let the caller pause.
				p.quarantine(provider, kind, err)
				excluded[provider.ID()] = true
				lastErr = err
				if next := p.nextProvider(excluded); next != nil {
					continue
				}
				return zero, provider.ID(), fmt.Errorf("%w（最近错误：%v）", ErrAllExhausted, lastErr)
			}
			delay := time.Duration(2*throttled) * time.Second
			if seconds, ok := resetHint(err); ok {
				delay = time.Duration(seconds) * time.Second
			}
			if delay > time.Minute {
				delay = time.Minute
			}
			if delay <= 0 {
				delay = time.Second
			}
			p.recordFailure(provider.ID(), kind, fmt.Errorf("并发降到 %d 后等待 %s 重试：%w", after, delay, err))
			if waitErr := p.wait(ctx, delay); waitErr != nil {
				return zero, provider.ID(), waitErr
			}
			lastErr = err
			continue
		}

		if !kind.Swappable() || (emitted != nil && *emitted) {
			if emitted != nil && *emitted && kind.Swappable() {
				// Interrupted mid-stream: the source is unusable for a while but
				// the answer is already partial, so stop instead of replaying.
				provider.quota.markExhausted(p.now(), p.now().Add(time.Minute), "流式输出中断："+kind.String())
			}
			return zero, provider.ID(), err
		}
		p.quarantine(provider, kind, err)
		excluded[provider.ID()] = true
		lastErr = err
		next := p.nextProvider(excluded)
		if next == nil {
			return zero, provider.ID(), fmt.Errorf("%w（最近错误：%v）", ErrAllExhausted, lastErr)
		}
		p.notifySwitch(provider.ID(), next.ID(), fmt.Sprintf("%s：%v", kind.String(), err))
	}
}

// wait is the backoff primitive; tests replace it to keep 429 retries instant.
func (p *Pool) wait(ctx context.Context, delay time.Duration) error {
	p.mu.RLock()
	fn := p.waiter
	p.mu.RUnlock()
	if fn != nil {
		return fn(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// nextProvider returns the preferred usable provider that is not excluded.
// The active provider keeps its slot while it has headroom, which protects the
// upstream prompt cache that makes long audits affordable.
func (p *Pool) nextProvider(excluded map[string]bool) *Provider {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// p.mu is held here, so the clock must be read directly.
	clock := p.clock
	active := p.find(p.active)
	if active != nil && !excluded[active.ID()] && active.Enabled() && active.quota.Ready(now) && clockOpenAt(clock, active.ID(), now) {
		threshold := p.routing.SwitchBelow
		if threshold <= 0 {
			threshold = 0.15
		}
		dwell := time.Duration(p.routing.MinDwellSeconds) * time.Second
		if active.quota.Ratio(now) >= threshold || dwell <= 0 || now.Sub(p.activeSince) < dwell {
			return active
		}
	}
	candidates := p.orderLocked(excluded)
	for _, candidate := range candidates {
		if !candidate.quota.Ready(now) || !clockOpenAt(clock, candidate.ID(), now) {
			continue
		}
		if candidate.ID() != p.active {
			previous := p.active
			p.active, p.activeSince = candidate.ID(), now
			go p.notifySwitch(previous, candidate.ID(), "余量不足后切换")
		}
		return candidate
	}
	return nil
}

func (p *Pool) hasReadyProvider() bool {
	now := p.now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	clock := p.clock // p.mu.RLock is held
	for _, provider := range p.providers {
		if provider.Enabled() && provider.quota.Ready(now) && clockOpenAt(clock, provider.ID(), now) {
			return true
		}
	}
	return false
}

func (p *Pool) quarantine(provider *Provider, kind FailureKind, err error) {
	now := p.now()
	if seconds, ok := resetHint(err); ok {
		delay := time.Duration(seconds) * time.Second
		if kind == FailureRateLimited {
			provider.quota.markRateLimited(now, delay, describeFailure(kind, provider.ID(), err))
		} else {
			provider.quota.markExhausted(now, now.Add(delay), describeFailure(kind, provider.ID(), err))
		}
		return
	}
	if kind == FailureRateLimited {
		provider.quota.markRateLimited(now, 60*time.Second, describeFailure(kind, provider.ID(), err))
		return
	}
	provider.quota.markExhausted(now, now.Add(provider.quota.windowSeconds()), describeFailure(kind, provider.ID(), err))
}

func (p *Pool) markSuccess(id string) {
	p.mu.Lock()
	delete(p.lastErr, id)
	p.mu.Unlock()
}

func (p *Pool) recordFailure(id string, kind FailureKind, err error) {
	p.mu.Lock()
	if p.lastErr == nil {
		p.lastErr = map[string]string{}
	}
	p.lastErr[id] = fmt.Sprintf("[%s] %v", kind.String(), err)
	p.mu.Unlock()
}

// affinityContext drops a Responses continuation that belongs to a different
// provider. Vendor continuation ids are not portable between vendors.
func (p *Pool) affinityContext(ctx context.Context, providerID string) context.Context {
	previous := llm.ResponseContinuationID(ctx)
	if previous == "" {
		return ctx
	}
	p.mu.RLock()
	owner := p.lineage[previous]
	p.mu.RUnlock()
	if owner == "" || owner == providerID {
		return ctx
	}
	return llm.DropResponseContinuation(ctx)
}

// rememberLineage records which provider produced a continuation id.
func (p *Pool) rememberLineage(responseID, providerID string) {
	if responseID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lineage == nil {
		p.lineage = map[string]string{}
	}
	if len(p.lineage) >= maxLineageEntries {
		p.lineage = map[string]string{}
	}
	p.lineage[responseID] = providerID
}

// MarkExhausted lets the agent layer report a quota failure seen outside the
// pool (for example a compression lane) so routing reacts to it.
func (p *Pool) MarkExhausted(id string, err error) {
	provider := p.find(id)
	if provider == nil {
		return
	}
	p.quarantine(provider, FailureBilling, err)
}

// TryFailover moves off the given provider if another enabled source is ready.
// It reports whether work may continue right now.
func (p *Pool) TryFailover(from string, err error) bool {
	provider := p.find(from)
	if provider != nil {
		p.quarantine(provider, classifyFailure(err), err)
	}
	return p.hasReadyProvider()
}
