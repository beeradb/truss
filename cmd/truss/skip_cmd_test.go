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
func skipFixture(t *testing.T, sha string) (*fakeLedger, *fakeTelegram, func(overrides map[string]string) func(string) string) {
	t.Helper()
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("failed/"+sha, []byte(`{"reason":"tofu apply failed for platform","at":"2026-09-08T12:30:45Z"}`))
	fl.put("head", []byte("someotherhead"))

	// skip announces before it acts, so the fixture needs somewhere for the
	// announcement to land. Without the base-URL override this suite would
	// post to the real Telegram on every run -- the exact bug the Cloudflare
	// probe had before it grew one.
	write(itemTelegram, fieldTelegramBotToken, "fake-bot-token")
	write(itemTelegram, fieldTelegramChatID, "-100200300")
	ft := newFakeTelegram(t)

	workdir := t.TempDir()
	envFn := func(overrides map[string]string) func(string) string {
		all := map[string]string{"TELEGRAM_API_BASE_URL": ft.srv.URL}
		for k, v := range overrides {
			all[k] = v
		}
		return testFullEnv(dir.Root, workdir, all)
	}
	return fl, ft, envFn
}

func TestSkipHappyPathAdvancesHeadAndWritesRecord(t *testing.T) {
	sha := "deadbeef"
	fl, _, envFn := skipFixture(t, sha)
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
	fl, _, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWhitespaceOnlyReason(t *testing.T) {
	sha := "deadbeef"
	fl, _, envFn := skipFixture(t, sha)
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
	fl, _, envFn := skipFixture(t, sha)
	env := envFn(nil) // TRUSS_SKIP_I_UNDERSTAND absent

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "known bad plan"}, env, nil, &stdout, &stderr)
	assertGuardRefused(t, fl, sha, code, stderr.String())
}

func TestSkipRefusesWhenConfirmationEnvVarNamesTheWrongSha(t *testing.T) {
	sha := "deadbeef"
	fl, _, envFn := skipFixture(t, sha)
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
	fl, _, envFn := skipFixture(t, sha)
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

// TestSkipAnnouncesBeforeItActs pins the order, not just the fact. The
// announcement is what makes this command survivable: threat-model.md says of
// the existing escape hatch that an operator "can never do it quietly", and
// skip is a second, narrower hatch aimed at one commit.
func TestSkipAnnouncesBeforeItActs(t *testing.T) {
	sha := "deadbeef"
	fl, ft, envFn := skipFixture(t, sha)
	env := envFn(map[string]string{"TRUSS_SKIP_I_UNDERSTAND": sha})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "plan can never apply"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	ft.mu.Lock()
	sent := ft.last
	ft.mu.Unlock()
	if sent == "" {
		t.Fatal("nothing was announced; a skip nobody is told about is the one thing this command must not be")
	}
	for _, want := range []string{sha, "SKIPPED", "plan can never apply"} {
		if !strings.Contains(sent, want) {
			t.Errorf("announcement = %q, want it to contain %q", sent, want)
		}
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Error("the skip was announced but not recorded")
	}
}

// TestSkipRefusesWhenItCannotAnnounce is the half that makes the ordering
// mean something. If the alert cannot be sent, nothing is written and HEAD
// does not move: a skip that could not be announced did not happen.
func TestSkipRefusesWhenItCannotAnnounce(t *testing.T) {
	sha := "deadbeef"
	fl, _, envFn := skipFixture(t, sha)
	// A host that cannot resolve, so Send fails at connect. A reserved
	// .invalid name rather than a loopback address: scripts/leakscan refuses
	// an IP literal anywhere in this repository, and cannot tell a test's
	// black hole from somebody's real cluster.
	env := envFn(map[string]string{
		"TRUSS_SKIP_I_UNDERSTAND": sha,
		"TELEGRAM_API_BASE_URL":   "http://truss-skip-test-unreachable.invalid:1",
	})

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"skip", sha, "--reason", "plan can never apply"}, env, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Error("a record was written even though the skip could not be announced")
	}
	if head, _ := fl.get("head"); string(head) != "someotherhead" {
		t.Errorf("head = %q, want it unmoved", head)
	}
}
