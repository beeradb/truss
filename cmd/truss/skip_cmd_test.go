package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// skipFixture wires a fake ledger with a failed/<sha> record already
// present and HEAD parked somewhere else -- the state every guard test
// below starts from, so each test can violate exactly one guard and leave
// every other guard satisfied.
func skipFixture(t *testing.T, sha string) (*fakeLedger, func(overrides map[string]string) func(string) string) {
	t.Helper()
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("failed/"+sha, []byte(`{"reason":"tofu apply failed for platform","at":"2026-09-08T12:30:45Z"}`))
	fl.put("head", []byte("someotherhead"))

	workdir := t.TempDir()
	envFn := func(overrides map[string]string) func(string) string {
		return testFullEnv(dir.Root, workdir, overrides)
	}
	return fl, envFn
}

func TestSkipHappyPathAdvancesHeadAndWritesRecord(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan, never applies"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	rec, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatal("no record written to applied/" + sha)
	}
	body := string(rec)
	if want := `"skipped":true`; !strings.Contains(body, want) {
		t.Errorf("record = %s, want it to contain %s", body, want)
	}
	if want := "known bad plan, never applies"; !strings.Contains(body, want) {
		t.Errorf("record = %s, want it to contain the reason %q", body, want)
	}

	head, ok := fl.get("head")
	if !ok || string(head) != sha {
		t.Errorf("head = %q, %v, want %q, true -- HEAD did not advance", head, ok, sha)
	}
}

func TestSkipRefusesMissingReason(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWhitespaceOnlyReason(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "   "}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWithoutAFailedRecord(t *testing.T) {
	sha := "neverattempted"
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("head", []byte("someotherhead"))
	// Deliberately no failed/<sha> record: the applier never tried this
	// commit.
	env := testFullEnv(dir.Root, t.TempDir(), map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWhenShaIsAlreadyHead(t *testing.T) {
	sha := "deadbeef"
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("failed/"+sha, []byte(`{"reason":"tofu apply failed for platform","at":"2026-09-08T12:30:45Z"}`))
	fl.put("head", []byte(sha))
	env := testFullEnv(dir.Root, t.TempDir(), map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Error("a record was written even though the guard should have refused first")
	}
	if head, _ := fl.get("head"); string(head) != sha {
		t.Errorf("head = %q, want it unchanged at %q", head, sha)
	}
}

func TestSkipRefusesWithoutTheConfirmationEnvVar(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	env := envFn(nil) // TRUSS_SKIP_I_UNDERSTAND absent

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWhenConfirmationEnvVarNamesTheWrongSha(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": "someothersha"})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

// TestSkipWritesTheRecordBeforeAdvancingHead proves the write ORDER: the
// skipped record must survive even when the following AdvanceHead fails,
// because a crash between the two writes must leave an explained commit
// behind, never an unexplained jump.
func TestSkipWritesTheRecordBeforeAdvancingHead(t *testing.T) {
	sha := "deadbeef"
	fl, envFn := skipFixture(t, sha)
	fl.failPutOn("head")
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 -- HEAD write failed after all guards passed (stderr: %s)", code, stderr.String())
	}

	rec, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatal("the skipped record did not survive the failed HEAD write")
	}
	if want := `"skipped":true`; !strings.Contains(string(rec), want) {
		t.Errorf("record = %s, want it to contain %s", rec, want)
	}

	if head, _ := fl.get("head"); string(head) != "someotherhead" {
		t.Errorf("head = %q, want it unchanged since the write failed", head)
	}
}

// assertGuardRefused is the shared shape every guard test above checks: a
// refusal with exit 1, HEAD untouched, and no record written -- "all
// guards refuse before anything is written".
func assertGuardRefused(t *testing.T, fl *fakeLedger, sha string, code int, stderrText string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderrText)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Error("a record was written even though a guard should have refused first")
	}
	if head, _ := fl.get("head"); string(head) != "someotherhead" {
		t.Errorf("head = %q, want it unchanged at %q", head, "someotherhead")
	}
}
