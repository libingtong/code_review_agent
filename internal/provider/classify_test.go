package provider

import (
	"fmt"
	"testing"
)

func TestClassifyVendorRateLimitSpellings(t *testing.T) {
	cases := []string{
		`openai native response.failed: rate_limit_exceeded 您的账户已达到速率限制，请您控制请求频率[2026...]`,
		`openai status 429: too many requests`,
		`rate limit exceeded`,
		`ratelimit reached`,
		`请求频率过快`,
	}
	for _, text := range cases {
		if got := Classify(fmt.Errorf("%s", text)); got != FailureRateLimited {
			t.Fatalf("%q classified as %v", text, got)
		}
	}
	if got := Classify(fmt.Errorf("openai status 500: internal")); got == FailureRateLimited {
		t.Fatalf("500 must not be rate limited")
	}
}
