package main

import (
	"context"
	"testing"
)

func TestHeadlessExitCodeReportsIncompleteTeam(t *testing.T) {
	if got := headlessExitCode("audit", nil, false, 1, 2); got != 1 {
		t.Fatalf("incomplete team exit=%d", got)
	}
	if got := headlessExitCode("completed", nil, false, 1, 2); got != 0 {
		t.Fatalf("completed team exit=%d", got)
	}
	if got := headlessExitCode("audit", context.DeadlineExceeded, false, 1, 2); got != 130 {
		t.Fatalf("deadline exit=%d", got)
	}
	if got := headlessExitCode("audit", nil, true, 0, 0); got != 4 {
		t.Fatalf("blocked exit=%d", got)
	}
}
