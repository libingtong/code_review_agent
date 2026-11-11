package agent

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"code-review-agent/internal/tools"
)

func TestPendingAuditCandidateDirectivePrioritizesExplicitPositiveConclusions(t *testing.T) {
	snapshot := tools.Snapshot{Flows: []tools.FlowReview{
		{Name: "callback-ownership", Status: "confirmed", Entry: "POST /callback", Evidence: "request ID reaches unscoped update", NextStep: "verify callback scope"},
		{Name: "reset-account-binding", Status: "vulnerable", Entry: "POST /reset", Evidence: "independent token and account queries"},
		{Name: "unverified-secret", Status: "suspicious"},
	}}
	before := snapshot
	text := pendingAuditCandidateDirective(snapshot)
	for _, want := range []string{"callback-ownership", "reset-account-binding", "POST /callback", "request ID reaches unscoped update", "verify callback scope", "verify_finding", "report_finding", "flow_review_delete", "具体缺口", "优先收敛第一条"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in directive %q", want, text)
		}
	}
	if strings.Contains(text, "unverified-secret") {
		t.Fatal("suspicious flow was promoted to a confirmed candidate")
	}
	if !reflect.DeepEqual(snapshot, before) {
		t.Fatal("directive changed audit state")
	}
}

func TestPendingAuditCandidateDirectiveDoesNotGuessFindingAssociation(t *testing.T) {
	snapshot := tools.Snapshot{
		Findings: []tools.Finding{{Title: "credentials in handler", Path: "handler.go"}},
		Flows:    []tools.FlowReview{{Name: "callback-auth-bypass", Status: "confirmed", Files: []string{"handler.go"}}},
	}
	text := pendingAuditCandidateDirective(snapshot)
	if !strings.Contains(text, "callback-auth-bypass") || !strings.Contains(text, "已有 1 条 finding") || !strings.Contains(text, "不同根因") {
		t.Fatalf("same-file finding hid another candidate or omitted dedup instruction: %q", text)
	}
}

func TestPendingAuditCandidateDirectiveIgnoresUnconfirmedAndClosedFlows(t *testing.T) {
	for _, status := range []string{"", "pending", "suspicious", "safe", "rejected", "excluded", "closed", "reviewed"} {
		if got := pendingAuditCandidateDirective(tools.Snapshot{Flows: []tools.FlowReview{{Name: "candidate", Status: status}}}); got != "" {
			t.Fatalf("status %q produced positive directive: %q", status, got)
		}
	}
	if got := pendingAuditCandidateDirective(tools.Snapshot{}); got != "" {
		t.Fatal("empty snapshot produced directive")
	}
}

func TestPendingAuditCandidateDirectiveBoundsItemsAndUnicode(t *testing.T) {
	long := strings.Repeat("证据\n", 10000)
	snapshot := tools.Snapshot{}
	for i := 0; i < 100; i++ {
		name := long
		if i == 3 {
			name = "fourth-candidate-must-not-appear"
		}
		snapshot.Flows = append(snapshot.Flows, tools.FlowReview{Name: name, Status: " confirmed ", Entry: long, Evidence: long, NextStep: long})
	}
	text := pendingAuditCandidateDirective(snapshot)
	if len(text) > 6144 || !utf8.ValidString(text) || strings.Contains(text, "fourth-candidate-must-not-appear") {
		t.Fatalf("invalid bounded directive: bytes=%d validUTF8=%v", len(text), utf8.ValidString(text))
	}
	if !strings.Contains(text, "100 条") || strings.Count(text, "已存证据：") != 3 {
		t.Fatal("candidate count or three-item excerpt changed")
	}
}
