package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestWhyAppliedRecordNamesEachRootsCount: an applied record with two
// roots -- one with a real count, one with a null resource_changes (the
// summary_from_plan fallback for an unparseable `tofu show -json`) -- must
// print a real number for the first and "unknown" for the second, never 0.
func TestWhyAppliedRecordNamesEachRootsCount(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"roots":{"platform":{"resource_changes":3},"credentials":{"resource_changes":null}}}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "platform") || !strings.Contains(out, "3 resource change") {
		t.Errorf("stdout = %q, want it to name platform's 3 resource changes", out)
	}
	if !strings.Contains(out, "credentials") || !strings.Contains(out, "unknown") {
		t.Errorf("stdout = %q, want it to name credentials as unknown, not 0", out)
	}
	if strings.Contains(out, "credentials: 0") {
		t.Errorf("stdout = %q, printed 0 for a null resource_changes -- must print unknown", out)
	}
}

// TestWhyNoopRecordExitsZero: `{"noop":true}` is a clean pass that
// touched no root.
func TestWhyNoopRecordExitsZero(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"noop":true}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "noop") {
		t.Errorf("stdout = %q, want it to say noop", stdout.String())
	}
}

// TestWhySkippedRecordExitsZeroAndPrintsReason: a `truss skip` record
// lives under the applied key too, tagged "skipped":true.
func TestWhySkippedRecordExitsZeroAndPrintsReason(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("applied/deadbeef", []byte(`{"skipped":true,"reason":"known bad plan, never applies","at":"2026-09-08T12:30:45Z"}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "known bad plan, never applies") {
		t.Errorf("stdout = %q, want it to print the skip reason", out)
	}
	if !strings.Contains(strings.ToLower(out), "skip") {
		t.Errorf("stdout = %q, want it to say this was a skip", out)
	}
}

// TestWhyFailedRecordExitsZeroAndPrintsReasonAndTimestamp: nothing under
// applied/, but a failed/ record exists.
func TestWhyFailedRecordExitsZeroAndPrintsReasonAndTimestamp(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("failed/deadbeef", []byte(`{"reason":"tofu apply failed for platform","at":"2026-09-08T12:30:45Z"}`))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "deadbeef"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tofu apply failed for platform") {
		t.Errorf("stdout = %q, want it to print the failure reason", out)
	}
	if !strings.Contains(out, "2026-09-08T12:30:45Z") {
		t.Errorf("stdout = %q, want it to print the recorded timestamp", out)
	}
}

// TestWhyAbsentRecordExitsTwo: neither key exists -- the queue has not
// reached this commit yet, matching `ledger get`'s own "exit 2 if absent".
func TestWhyAbsentRecordExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"why", "neverseen"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}
