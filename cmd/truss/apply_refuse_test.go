package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestApplyRefusesToRunWithoutAValidConfig checks the boot-time refusal
// path (§2 item 5's neighbour: an invalid config is refused before any
// credential, ledger entry or git remote is touched, exactly like a
// missing HEAD is). Any one of the ten required variables missing must
// stop the pass at config.Load, before it does anything else -- exit 1,
// every problem named on stderr, nothing on stdout.
func TestApplyRefusesToRunWithoutAValidConfig(t *testing.T) {
	dir, _ := testSecretsDir(t)
	env := testFullEnv(dir.Root, t.TempDir(), map[string]string{
		"REPO": "", // one required variable, deliberately unset
	})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"apply"}, env, nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty -- a config refusal must not reach the JSON/report channel", stdout.String())
	}
	if !strings.Contains(stderr.String(), "REPO") {
		t.Fatalf("stderr = %q, want it to name the missing $REPO", stderr.String())
	}

	// Every problem is reported, not just the first (config.Load's own
	// contract) -- unset a second variable and confirm both are named.
	env2 := testFullEnv(dir.Root, t.TempDir(), map[string]string{
		"REPO":     "",
		"APPROVER": "",
	})
	var stdout2, stderr2 bytes.Buffer
	code2 := runEnv(context.Background(), []string{"apply"}, env2, nil, &stdout2, &stderr2)
	if code2 != 1 {
		t.Fatalf("exit code = %d, want 1", code2)
	}
	if !strings.Contains(stderr2.String(), "REPO") || !strings.Contains(stderr2.String(), "APPROVER") {
		t.Fatalf("stderr = %q, want both $REPO and $APPROVER named", stderr2.String())
	}
}
