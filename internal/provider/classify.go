package provider

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"code-review-agent/internal/llm"
)

// ErrAllExhausted means every enabled source refused for quota reasons. The
// caller must pause and resume later instead of treating it as a crash.
var ErrAllExhausted = errors.New("所有源余量均不足")

type FailureKind int

const (
	FailureOther FailureKind = iota
	FailureBilling
	FailureRateLimited
	FailureAuth
	FailureContext
	FailureNetwork
	FailureProtocol
)

func (k FailureKind) String() string {
	switch k {
	case FailureBilling:
		return "billing"
	case FailureRateLimited:
		return "rate_limited"
	case FailureAuth:
		return "auth"
	case FailureContext:
		return "context"
	case FailureNetwork:
		return "network"
	case FailureProtocol:
		return "protocol"
	}
	return "other"
}

// Swappable reports whether another provider could plausibly satisfy the same
// request. Auth and context errors are configuration/limits problems: retrying
// them elsewhere just burns another source's quota.
func (k FailureKind) Swappable() bool {
	return k == FailureBilling || k == FailureRateLimited
}

var billingMarkers = []string{
	"insufficient", "余额不足", "余额已耗尽", "额度不足", "额度已用尽", "欠费", "payment required",
	"credit balance", "credits exhausted", "quota exceeded", "quota_exceeded", "billing_hard_limit",
	"exceeded your current quota", "openai status 402:", "usage limit reached", "5h limit",
	"limit reached", "套餐额度", "额度耗尽",
}

var authMarkers = []string{
	"openai status 401:", "openai status 403:", "invalid api key", "incorrect api key",
	"unauthorized", "authentication", "no permission", "invalid token",
}

// classifyFailure inspects only API error text. Generated model text and tool
// output are never classified: they are untrusted content, not transport state.
func classifyFailure(err error) FailureKind {
	if err == nil {
		return FailureOther
	}
	if errors.Is(err, ErrAllExhausted) {
		return FailureBilling
	}
	if errors.Is(err, llm.ErrToolProtocol) {
		return FailureProtocol
	}
	text := strings.ToLower(err.Error())
	if isContextLength(text) {
		return FailureContext
	}
	if hasMarker(text, authMarkers) {
		return FailureAuth
	}
	if hasMarker(text, billingMarkers) {
		return FailureBilling
	}
	if strings.Contains(text, "openai status 429") || strings.Contains(text, "rate limit") || strings.Contains(text, "too many requests") {
		return FailureRateLimited
	}
	var network net.Error
	if errors.As(err, &network) {
		return FailureNetwork
	}
	if strings.Contains(text, "connection reset") || strings.Contains(text, "connection refused") ||
		strings.Contains(text, "broken pipe") || errors.Is(err, llm.ErrIncompleteGeneration) {
		return FailureNetwork
	}
	return FailureOther
}

func isContextLength(text string) bool {
	return strings.Contains(text, "maximum context length") || strings.Contains(text, "context length") ||
		strings.Contains(text, "input_tokens") || strings.Contains(text, "context_length_exceeded")
}

func hasMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// resetHint extracts a retry/reset delay hint from provider error text so a
// paused run can wake up at the right moment instead of polling blindly.
func resetHint(err error) (seconds int, ok bool) {
	if err == nil {
		return 0, false
	}
	text := strings.ToLower(err.Error())
	for _, key := range []string{"retry-after:", "retry after ", "retry_after", "reset after ", "resets in ", "reset_at"} {
		index := strings.Index(text, key)
		if index < 0 {
			continue
		}
		rest := text[index+len(key):]
		number := 0
		digits := 0
		for _, r := range rest {
			if r < '0' || r > '9' {
				if digits > 0 {
					break
				}
				continue
			}
			number = number*10 + int(r-'0')
			digits++
			if digits > 8 {
				break
			}
		}
		if digits > 0 && number > 0 {
			return number, true
		}
	}
	return 0, false
}

func describeFailure(kind FailureKind, providerID string, err error) string {
	return fmt.Sprintf("%s 源 %s 失败：%v", providerID, kind.String(), err)
}

// Classify exposes failure classification to the agent retry layer without
// duplicating vendor knowledge there.
func Classify(err error) FailureKind { return classifyFailure(err) }

// ResetHint exposes retry/reset hints parsed from provider error text.
func ResetHint(err error) (int, bool) { return resetHint(err) }
