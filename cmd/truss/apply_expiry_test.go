package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAnUnusableExpirySweepIsReportedAndDoesNotFailThePass pins the shape
// chosen on 2026-09-08 after both reviews: the sweep refuses to claim a
// clean bill it did not earn, so its problem must REACH somebody -- but it
// is not a failure, so it must not turn a clean pass red.
//
// ⚠️ IT USED TO SET failure, AND THAT WOULD HAVE MADE EVERY PRODUCTION PASS
// RED. Nothing seeds `expires` into Vault yet, so "lists N items but not one
// records an expiry" fires on every run: exit 1 plus a Telegram FAILED every
// five minutes, ~288 a day. An alert channel nobody reads is where a real
// digest-gate refusal goes to die, which is why both reviewers called it a
// security cost rather than noise.
func TestAnUnusableExpirySweepIsReportedAndDoesNotFailThePass(t *testing.T) {
	const sha = "commitsha2"

	deps, _, ft, _ := gateDeps(t, sha, sha)
	// The vault is production-shaped (items, none with an expires), so the
	// sweep genuinely cannot report -- see productionShapedVault.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, sha)

	// The pass refuses at the digest gate (no digest seeded here), and that
	// is the only reason it fails: the sweep must not have contributed.
	if strings.Contains(result.failure, "expiry") {
		t.Errorf("result.failure names the expiry sweep, which must not fail the pass: %q", result.failure)
	}

	text := ft.lastText()
	if text == "" {
		t.Fatal("no message reached the fake Telegram server")
	}
	if !strings.Contains(text, "EXPIRY NOT CHECKED") {
		t.Errorf("the alert does not report the unusable sweep, so nobody learns of it: %q", text)
	}
	if !strings.Contains(text, "not one records an expiry") {
		t.Errorf("the alert does not say WHY the sweep could not report: %q", text)
	}
}

// TestACleanPassStaysCleanWithAnUnusableSweep is the half that matters most:
// with the vault shaped as production is today, a pass that has nothing else
// wrong with it exits 0 and does not send FAILED.
func TestACleanPassStaysCleanWithAnUnusableSweep(t *testing.T) {
	const sha = "commitsha2"

	deps, fl, ft, _ := gateDeps(t, sha, sha)
	fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(ourDigest(t)))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- an unusable expiry sweep is not a failed pass", result.failure)
	}
	text := ft.lastText()
	if strings.Contains(text, "FAILED") {
		t.Errorf("the alert leads with FAILED on an otherwise clean pass: %q", text)
	}
	if !strings.Contains(text, "EXPIRY NOT CHECKED") {
		t.Errorf("the sweep's problem was swallowed entirely: %q", text)
	}
}

// TestADriftRunStillClonesTheRepository covers a regression that shipped and
// that NOTHING in this suite or in internal/parity could see: EnsureClone and
// Fetch lived inside runCommitLoop, which a drift run skips entirely, so a
// drift-only pass never cloned and every checkout it then attempted failed
// with "chdir /work/repo: no such file or directory".
//
// apply.sh calls ensure_workdir unconditionally at top level (apply.sh:
// 207-212), before its own DRIFT_ONLY branch, which is why the bash's drift
// job works.
//
// ⚠️ IT WAS FOUND BY THE FIRST SHADOW RUN AGAINST THE REAL CLUSTER, not by a
// test, and the reason is worth keeping: every fake git succeeds whether or
// not a clone happened, so "check out a ref in a directory that does not
// exist" has no counterpart in a fake. This test therefore asserts the CALL,
// which is the only thing a fake can observe.
func TestADriftRunStillClonesTheRepository(t *testing.T) {
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{DirsAtRef: map[string][]string{"": {"platform"}, head: {"platform"}}}
	tofu := &fakeTofu{}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	deps.Cfg.DriftOnly = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, head)

	if !git.cloned() {
		t.Error("a drift run did not clone the repository, so every checkout it makes will fail")
	}
	if !git.fetched() {
		t.Error("a drift run did not fetch origin main, so it plans against a stale tree")
	}
	if git.tokenSeen() == "" {
		t.Error("a drift run never attached the installation token, so the clone is unauthenticated")
	}
}
