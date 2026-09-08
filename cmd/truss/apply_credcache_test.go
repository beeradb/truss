package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestOneCredentialCachePerPass pins FIX 3: credCache used to be built
// separately inside runCommitLoop AND runRotation, so before the rotation
// fix (which now makes the two mutually exclusive within one real pass --
// see apply_daily_pass_test.go) a pass reaching both read Google's key, the
// state-encryption passphrase and the Cloudflare mint token from the mount
// TWICE. The bash reads this batch at most once per PROCESS
// (APPLY_CREDS_READ, apply.sh:255-275); credCache's own doc already claimed
// "at most once per pass" and this test is what makes that true.
//
// Because runCommitLoop and runRotation no longer run together inside a
// real runApplyPass call, this test drives them directly, back to back,
// sharing one *credCache the way runApplyPass now builds and hands one to
// whichever of them it calls. That is a deliberate, synthetic pairing: it
// isolates FIX 3 (does a shared cache actually get reused) from FIX 1
// (which of the two gets called on a given pass), which is what lets this
// test fail on FIX 3 alone -- see the negative-test note in the PR/report.
//
// runRotation no longer takes a credentialsAppliedAt argument -- the
// "already applied this run" short-circuit it used to gate on was removed
// as unreachable (runCommitLoop and runRotation never run in the same real
// pass any more; see the comment where the branch used to be). So calling
// it here, right after runCommitLoop applied credentials/, tests the shared
// cache with nothing left to short-circuit around.
//
// ⚠️ HOW THE READS ARE COUNTED. secrets.Dir is a concrete struct
// (filepath.Join + os.Stat + os.ReadFile), not an interface, so there is no
// seam to substitute a counting fake without either reimplementing Dir's
// own logic (risking a fixture that quietly diverges from production) or
// changing applyDeps.Dir to an interface everywhere (a far bigger change
// than this fix). Instead Dir carries an OnFieldRead hook, added alongside
// this test for exactly this purpose: it fires once per successful Field
// read, straight out of the real Dir.Field, so the count is of actual
// filesystem reads and not of anything this test asserts by construction.
//
// The hook is a FIELD on Dir rather than a package-level var, so there is no
// global to reset and no way for one test to observe another's writes.
func TestOneCredentialCachePerPass(t *testing.T) {
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{
		CommitsList:       []string{head},
		ChangedByCommit:   map[string][]string{head: {"credentials/main.tf"}},
		TreeRootsByCommit: map[string][]string{head: nil},
		HasDirFn:          func(string) bool { return true },
	}
	tofu := &fakeTofu{}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	// runApplyPass normally mints this token before either function runs;
	// this test calls them directly, below that layer, so it sets one by
	// hand the same way runApplyPass would have.
	deps.Token = "gh-fixture" // short: leakscan refuses a credential-shaped literal

	var mu sync.Mutex
	reads := map[string]int{}
	deps.Dir.OnFieldRead = func(item, field string) {
		mu.Lock()
		reads[item+"/"+field]++
		mu.Unlock()
	}

	cc := &credCache{dir: deps.Dir}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	newLast, applied, _, loopFailure, _ := runCommitLoop(ctx, deps, head, cc)
	if loopFailure != "" {
		t.Fatalf("runCommitLoop: %s", loopFailure)
	}
	if applied != 1 {
		t.Fatalf("runCommitLoop applied %d commits, want 1 -- the credentials root never got exercised", applied)
	}

	if _, _, _, err := runRotation(ctx, deps, newLast, cc); err != nil {
		t.Fatalf("runRotation: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{
		itemGCPApply + "/" + fieldGCPCredentials,
		itemTofuEncryption + "/" + fieldTofuPassphrase,
		itemCFTokenMint + "/" + fieldCFCredential,
	} {
		if got := reads[key]; got != 1 {
			t.Errorf("%s was read %d times across a pass that both applied and rotated with a shared credCache, want exactly 1", key, got)
		}
	}
}
