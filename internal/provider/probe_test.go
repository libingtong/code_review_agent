package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		err  error
		want FailureKind
	}{
		{errors.New("openai status 402: 余额不足"), FailureBilling},
		{errors.New("exceeded your current quota"), FailureBilling},
		{errors.New("openai status 429: rate limit exceeded"), FailureRateLimited},
		{errors.New("openai status 401: invalid api key"), FailureAuth},
		{errors.New("maximum context length exceeded"), FailureContext},
		{errors.New("connection reset by peer"), FailureNetwork},
		{llm.ErrToolProtocol, FailureProtocol},
		{errors.New("something odd"), FailureOther},
		{ErrAllExhausted, FailureBilling},
	}
	for _, test := range cases {
		if got := classifyFailure(test.err); got != test.want {
			t.Fatalf("%q: got %v, want %v", test.err, got, test.want)
		}
	}
	if FailureBilling.Swappable() != true || FailureAuth.Swappable() != false || FailureNetwork.Swappable() != false {
		t.Fatal("only quota-class failures may switch source")
	}
}

// GLM-shaped payload: a limits array selected by a type filter, plus a unix
// reset timestamp. This is the shape community probes observe today.
const glmQuotaFixture = `{
  "code": 200,
  "data": {
    "limits": [
      {"type": "TOKENS_LIMIT", "usage": 4200, "limit": 12000, "nextResetTime": 1794000000000},
      {"type": "WEEKLY_LIMIT", "usage": 20000, "limit": 60000, "nextResetTime": 1794500000000}
    ]
  }
}`

func TestSampleFromJSONWithFilterPath(t *testing.T) {
	bind := config.BindConfig{
		Used:  []string{"data.limits[type=TOKENS_LIMIT].usage"},
		Total: []string{"data.limits[type=TOKENS_LIMIT].limit"},
		Reset: []string{"data.limits[type=TOKENS_LIMIT].nextResetTime"},
	}
	sample, err := sampleFromJSON([]byte(glmQuotaFixture), bind, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Used != 4200 || sample.Total != 12000 {
		t.Fatalf("sample = %+v", sample)
	}
	if !sample.HasReset || sample.ResetAt.Year() < 2026 {
		t.Fatalf("reset not parsed from epoch millis: %v", sample.ResetAt)
	}
}

func TestCandidatePathsTolerFieldRenames(t *testing.T) {
	body := []byte(`{"data":{"used":10,"limits":[]}}`)
	bind := config.BindConfig{
		Used:  []string{"data.limits[type=TOKENS_LIMIT].usage", "data.used"},
		Total: []string{"data.limits[type=TOKENS_LIMIT].limit", "data.total"},
	}
	sample, err := sampleFromJSON(body, bind, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !sample.HasUsed || sample.Used != 10 || sample.HasTotal {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestSampleRejectsUnmappedPayload(t *testing.T) {
	if _, err := sampleFromJSON([]byte(`{"unexpected":true}`), config.BindConfig{Used: []string{"data.used"}}, time.Now()); err == nil {
		t.Fatal("unmapped payload must be an error so /quota can show the raw body")
	}
}

func TestPercentOutputFallsBackToRegex(t *testing.T) {
	sample, err := sampleFromRegex("5h window: used 62% (resets in 2h)", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !sample.HasUsed || sample.Used != 62 || sample.Total != 100 {
		t.Fatalf("sample = %+v", sample)
	}
	named := `used=(?P<used>[0-9.]+)% total=(?P<total>[0-9.]+)% reset=(?P<reset>[0-9]+h)`
	sample, err = sampleFromRegex("used=41.5% total=100% reset=3h", named, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Used != 41.5 || !sample.HasReset {
		t.Fatalf("named groups not honoured: %+v", sample)
	}
}

func TestHTTPProbeSendsBearerAndReportsStatus(t *testing.T) {
	var gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, `{"data":{"used":1,"total":2}}`)
	}))
	defer server.Close()
	entry := testProvider("glm", server.URL+"/v1", 0)
	entry.OpenAI.APIKey = "sk-secret"
	entry.Quota.Probe = config.ProbeConfig{Type: "http", URL: server.URL + "/quota", Headers: map[string]string{"Authorization": "Bearer ${api_key}"},
		Bind: config.BindConfig{Used: []string{"data.used"}, Total: []string{"data.total"}}, TimeoutSeconds: 5}
	probe := newProbe(entry)
	sample, raw, err := probe.Query(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret" || gotPath != "/quota" {
		t.Fatalf("probe request wrong: auth=%q path=%q", gotAuth, gotPath)
	}
	if sample.Used != 1 || !strings.Contains(raw, `"used":1`) {
		t.Fatalf("sample=%+v raw=%q", sample, raw)
	}
}

func TestHTTPProbeSurfacesErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer server.Close()
	entry := testProvider("glm", server.URL+"/v1", 0)
	entry.Quota.Probe = config.ProbeConfig{Type: "http", URL: server.URL + "/quota", TimeoutSeconds: 5,
		Bind: config.BindConfig{Used: []string{"data.used"}}}
	_, raw, err := newProbe(entry).Query(t.Context(), entry)
	if err == nil {
		t.Fatal("HTTP 401 must be an error")
	}
	if !strings.Contains(raw, "bad key") {
		t.Fatalf("raw body must be preserved for diagnostics, got %q", raw)
	}
}

func TestLocalProbeIsExplicitFallback(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 100)
	if _, _, err := newProbe(entry).Query(t.Context(), entry); err == nil {
		t.Fatal("local probe must report that it cannot read a vendor quota")
	}
}

func TestProbeAutoDetection(t *testing.T) {
	entry := testProvider("glm", "http://example.invalid", 0)
	if kind := newProbe(entry).Kind(); kind != "local" {
		t.Fatalf("no probe config should fall back to local, got %q", kind)
	}
	entry.Quota.Probe.URL = "https://example.invalid/quota"
	if kind := newProbe(entry).Kind(); kind != "http" {
		t.Fatalf("a url should select http, got %q", kind)
	}
	entry.Quota.Probe = config.ProbeConfig{Command: []string{"mmx", "quota"}}
	if kind := newProbe(entry).Kind(); kind != "command" {
		t.Fatalf("a command should select command probe, got %q", kind)
	}
}

func TestLookupPathEdgeCases(t *testing.T) {
	var root any
	_ = json.Unmarshal([]byte(`{"a":{"b":[{"k":"x","v":1},{"k":"y","v":2}]},"z":"3%"}`), &root)
	if value, ok := lookupPath(root, "a.b[k=y].v"); !ok || value.(float64) != 2 {
		t.Fatalf("filter lookup failed: %v %v", value, ok)
	}
	if value, ok := lookupPath(root, "a.b[0].k"); !ok || value.(string) != "x" {
		t.Fatalf("index lookup failed: %v %v", value, ok)
	}
	if _, ok := lookupPath(root, "a.missing"); ok {
		t.Fatal("missing path must not resolve")
	}
	if value, ok := lookupPath(root, "z"); !ok {
		t.Fatal("string value must resolve")
	} else if number, ok := asFloat(value); !ok || number != 3 {
		t.Fatalf("percent string parse failed: %v %v", number, ok)
	}
}

func TestVendorErrorEnvelopeIsSurfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":401,"msg":"令牌已过期或验证不正确","success":false}`)
	}))
	defer server.Close()
	entry := testProvider("glm", server.URL+"/v1", 0)
	entry.Quota.Probe = config.ProbeConfig{Type: "http", URL: server.URL + "/quota", TimeoutSeconds: 5,
		Bind: config.BindConfig{Used: []string{"data.used"}}}
	_, _, err := newProbe(entry).Query(t.Context(), entry)
	if err == nil || !strings.Contains(err.Error(), "令牌已过期") {
		t.Fatalf("vendor business error must be reported, got %v", err)
	}
}

// GLM's TOKENS_LIMIT only reports a percentage, so the total comes from a literal
// bind entry rather than from the payload.
func TestPercentagePayloadWithLiteralTotal(t *testing.T) {
	body := []byte(`{"code":200,"success":true,"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","percentage":1,"nextResetTime":1794033902188}]}}`)
	bind := config.BindConfig{
		Used:  []string{"data.limits[type=TOKENS_LIMIT].percentage"},
		Total: []string{"=100"},
		Reset: []string{"data.limits[type=TOKENS_LIMIT].nextResetTime"},
		Plan:  []string{"data.level"},
	}
	sample, err := sampleFromJSON(body, bind, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !sample.HasUsed || sample.Used != 1 || !sample.HasTotal || sample.Total != 100 {
		t.Fatalf("percent payload not parsed: %+v", sample)
	}
	if sample.Plan != "max" {
		t.Fatalf("plan not captured: %q", sample.Plan)
	}
	if !sample.HasReset || sample.ResetAt.Year() < 2026 {
		t.Fatalf("reset not parsed: %v", sample.ResetAt)
	}
}

// A real total in the payload must win over the literal fallback.
func TestRealTotalWinsOverLiteral(t *testing.T) {
	body := []byte(`{"data":{"used":4200,"total":28000}}`)
	bind := config.BindConfig{Used: []string{"data.used"}, Total: []string{"data.total", "=100"}}
	sample, err := sampleFromJSON(body, bind, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sample.Total != 28000 {
		t.Fatalf("literal overrode the real total: %v", sample.Total)
	}
}

func TestParseModelListShapes(t *testing.T) {
	cases := map[string][]string{
		`{"data":[{"id":"a"},{"id":"b"}]}`:        {"a", "b"},
		`{"models":["x","y"]}`:                    {"x", "y"},
		`{"models":[{"display_name":"glm-5.3"}]}`: {"glm-5.3"},
		`["p","q"]`:                           {"p", "q"},
		`{"models":[{"slug":"glm-5-turbo"}]}`: {"glm-5-turbo"},
	}
	for body, want := range cases {
		got, err := parseModelList([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v want %v", body, got, want)
		}
	}
	if _, err := parseModelList([]byte(`{"unexpected":true}`)); err == nil {
		t.Fatal("unparsable body must error")
	}
}
