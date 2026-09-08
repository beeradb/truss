package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/beeradb/truss/internal/plan"
)

// TestPlanDigestReadsStdinAndWritesNoTrailingNewline exercises the wire
// contract CI and the applier both rely on: `plan-digest` reads the whole
// plan from stdin and prints exactly the 64 lowercase hex characters
// plan.Digest returns, with NO trailing newline -- a byte a shell
// `$(...)` capture would strip silently, but a byte a ledger object
// compared for equality would not.
func TestPlanDigestReadsStdinAndWritesNoTrailingNewline(t *testing.T) {
	planJSON := `{"resource_changes":[{"address":"a","change":{"actions":["create"],"before":null,"after":{"x":1}}}]}`
	want, err := plan.Digest([]byte(planJSON))
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"plan-digest"}, strings.NewReader(planJSON), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if strings.HasSuffix(stdout.String(), "\n") {
		t.Fatalf("stdout ends with a newline: %q", stdout.String())
	}
	if len(stdout.String()) != 64 {
		t.Fatalf("stdout length = %d, want 64", len(stdout.String()))
	}
}

// TestPlanDigestOnBadInputRefuses confirms the failure path is a refusal
// (exit 1, message on stderr) rather than a zero-value digest that would
// look like a real one.
func TestPlanDigestOnBadInputRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"plan-digest"}, strings.NewReader("not json"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr is empty, want a message")
	}
}
