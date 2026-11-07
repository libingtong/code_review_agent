package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"code-review-agent/internal/applog"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/provider"
)

func waitModelRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Only transport/service availability failures enter the disconnect policy.
// Context budgets, invalid tool output and max_turns are never disconnects.
func isModelDisconnect(err error) bool {
	if err == nil || isContextLengthError(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "openai status 5") || strings.Contains(text, "openai status 408:") || strings.Contains(text, "openai status 429:") || strings.Contains(text, "stream ended without response.completed") || strings.Contains(text, "connection reset") || strings.Contains(text, "connection refused") || strings.Contains(text, "broken pipe")
}

// Inspect API errors only, never generated text or ordinary tool output.
func isBillingExhausted(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"insufficient", "余额不足", "余额已耗尽", "额度不足", "额度已用尽", "欠费", "payment required", "credit balance", "credits exhausted", "quota exceeded", "quota_exceeded", "billing_hard_limit", "exceeded your current quota", "openai status 402:"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

const (
	modelRetryInitialDelay = time.Second
	modelRetryMaxDelay     = 5 * time.Minute
)

// A network failure is retried until the request context is cancelled. The
// delay grows exponentially, but saturates before duration arithmetic can
// overflow. Billing exhaustion is handled separately and never retried.
func (a *Agent) modelRequest(ctx context.Context, emit func(Event), request func() (string, error)) (string, error) {
	wait := a.waitRetry
	if wait == nil {
		wait = waitModelRetry
	}
	incompleteRetries := 0
	retryDelay := modelRetryInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		answer, err := request()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err == nil {
			return answer, nil
		}
		if errors.Is(err, provider.ErrOutOfHours) {
			if a.onDisconnect != nil {
				a.onDisconnect(err)
			}
			return "", fmt.Errorf("不在工作时段，已暂停并保留进度，窗口开启后自动继续: %w", err)
		}
		if isBillingExhausted(err) {
			applog.L().Warnf("计费/额度错误，触发暂停：%v", err)
			if a.onDisconnect != nil {
				a.onDisconnect(err)
			}
			if errors.Is(err, provider.ErrAllExhausted) {
				return "", fmt.Errorf("所有启用源的 5 小时余量均已耗尽，团队已暂停并保留进度，余量恢复后自动继续: %w", err)
			}
			return "", fmt.Errorf("API余额或额度不足，已立即终止全队运行，不再重试: %w", err)
		}
		if provider.Classify(err) == provider.FailureRateLimited {
			applog.L().Warnf("限流 429：%v", err)
			// A 429 is a temporary vendor limit, not an outage: honour the reset
			// hint instead of sleeping through the generic network cap.
			delay := time.Minute
			if seconds, ok := provider.ResetHint(err); ok {
				delay = time.Duration(seconds) * time.Second
			}
			if emit != nil {
				emit(Event{Kind: "waiting", Content: fmt.Sprintf("模型服务限流；%s 后重试（切换源或暂停可停止等待）", delay)})
			}
			if err := wait(ctx, delay); err != nil {
				return "", err
			}
			continue
		}
		if errors.Is(err, llm.ErrIncompleteGeneration) {
			// Reasoning usually consumed the whole output budget; retry cheaply
			// instead of replaying the same oversized request.
			a.reducedReasoning = true
			if incompleteRetries == 3 {
				return "", fmt.Errorf("生成未完成，回退到上次完整结果后重试3次仍失败；保留进度，输入 go 可继续: %w", err)
			}
			incompleteRetries++
			if emit != nil {
				emit(Event{Kind: "info", Content: fmt.Sprintf("生成未完成：%v；丢弃本次残缺输出，回退到上次完整结果，重试 %d/3", err, incompleteRetries)})
			}
			// The caller commits model output only on success. Repeat generation
			// from that boundary, never replay the preceding successful tool.
			continue
		}
		if !isModelDisconnect(err) {
			applog.L().Errorf("模型请求失败（不重试）：%v", err)
			return "", err
		}
		applog.L().Warnf("模型连接异常，准备重试：%v", err)
		if emit != nil {
			emit(Event{Kind: "waiting", Content: fmt.Sprintf("模型服务断线；等待 %s 后继续重试（取消任务可停止）", retryDelay)})
		}
		if err := wait(ctx, retryDelay); err != nil {
			return "", err
		}
		if retryDelay < modelRetryMaxDelay {
			if retryDelay > modelRetryMaxDelay/2 {
				retryDelay = modelRetryMaxDelay
			} else {
				retryDelay *= 2
			}
		}
	}
}

// modelDisconnected pauses the whole team on quota exhaustion. With a provider
// pool the pause is resumable: the earliest 5h reset is recorded so the
// scheduler can wake the team up instead of waiting for a manual go.
func (t *Team) modelDisconnected(err error) {
	if errors.Is(err, provider.ErrOutOfHours) {
		var out *provider.OutOfHoursError
		reason, until := "不在工作时段", time.Time{}
		if errors.As(err, &out) {
			until = out.NextOpen
			if out.Detail != "" {
				reason += "（" + out.Detail + "）"
			}
		}
		t.mu.Lock()
		cancel := t.cancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		t.pauseForWindow(reason, until)
		notice := "不在工作时段：已暂停并保留进度。"
		if !until.IsZero() {
			notice = fmt.Sprintf("不在工作时段：已暂停并保留进度，预计 %s 自动继续。", until.Local().Format("01-02 15:04"))
		}
		t.publish(Event{Kind: "paused", Content: notice})
		t.publish(Event{Kind: "info", Content: notice})
		t.emitState()
		return
	}
	if !isBillingExhausted(err) {
		return
	}
	t.mu.Lock()
	cancel := t.cancel
	pool := t.pool
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	reason := "API余额或额度不足"
	var until time.Time
	if pool != nil {
		budget := pool.EarliestReset()
		if budget.EarliestReset.After(t.clock()) {
			reason, until = budget.Reason, budget.EarliestReset
			if budget.Detail != "" {
				reason += "（" + budget.Detail + "）"
			}
		}
	}
	t.mu.Lock()
	t.pauseReason, t.pauseUntil = reason, until
	t.mu.Unlock()
	notice := "API余额或额度不足：已暂停全队（包括管理员），保留进度，不重试。"
	if !until.IsZero() {
		notice = fmt.Sprintf("%s；预计 %s 恢复余量后自动继续（也可随时输入 go 手动继续）。", reason, until.Local().Format("01-02 15:04"))
	}
	t.publish(Event{Kind: "paused", Content: notice + err.Error()})
	t.publish(Event{Kind: "info", Content: notice})
	t.emitState()
}
