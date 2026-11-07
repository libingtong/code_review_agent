package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-review-agent/internal/applog"
)

// payloadKey carries the exact request body so a failed call can be dumped for
// offline inspection. Vendor protocol differences are invisible without it.
type payloadKey struct{}

func withPayload(ctx context.Context, payload any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, payloadKey{}, payload)
}

func payloadFrom(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(payloadKey{})
}

// reducedReasoningKey marks a request that should spend less output budget on
// thinking. Providers that bill reasoning against max_output_tokens otherwise
// reproduce the same truncation on every retry.
type reducedReasoningKey struct{}

// WithReducedReasoning asks the client to downgrade reasoning for this call.
func WithReducedReasoning(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, reducedReasoningKey{}, true)
}

func reducedReasoning(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	flagged, _ := ctx.Value(reducedReasoningKey{}).(bool)
	return flagged
}

// logModelFailure records a failed model call: always a warn line, plus a
// redacted payload dump when logging.llm_dump is on.
func logModelFailure(ctx context.Context, protocol, model string, err error) {
	logger := applog.L()
	if logger == nil || err == nil {
		return
	}
	logger.Warnf("llm %s model=%s error=%v", protocol, model, err)
	if !logger.LLMDump() {
		return
	}
	payload := payloadFrom(ctx)
	dump := map[string]any{
		"protocol": protocol,
		"model":    model,
		"error":    err.Error(),
	}
	// Prefer the raw request body: it is a struct/map that RedactValue can walk.
	if payload != nil {
		dump["request"] = asPlain(payload)
	}
	path, dumpErr := logger.DumpJSON("llm-"+protocol, dump)
	if dumpErr != nil {
		logger.Warnf("llm dump failed: %v", dumpErr)
		return
	}
	if path != "" {
		logger.Infof("llm failure dumped to %s", path)
	}
}

// asPlain converts a payload struct to a JSON-friendly value so redaction can
// walk it field by field.
func asPlain(payload any) any {
	switch typed := payload.(type) {
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		return decodeJSONValue(typed)
	}
}

func decodeJSONValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return string(data)
	}
	return decoded
}

// briefError trims long provider bodies for a single log line.
func briefError(err error, limit int) string {
	if err == nil {
		return ""
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit] + fmt.Sprintf("…(%d bytes)", len(text))
}
