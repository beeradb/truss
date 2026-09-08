package main

import (
	"context"
	"testing"
	"time"
)

// TestRotationTestsTheTreeItIsAboutToRotate covers an ordering bug no test
// could previously see. runRotation asked Git.HasDir("credentials") BEFORE
// checking out `last`, so it tested whatever tree the commit loop happened
// to leave behind rather than the tree it was about to rotate.
//
// Both directions bite: a false skip silently stalls a 45-day rotation
// window, and the inverse produces a hard failure in applyOneRoot for a
// commit that simply has no credentials root, which is a skip. runDrift
// already had the order right, so the inconsistency lived inside one file.
//
// ⚠️ THE FAKE IS WHY IT WAS INVISIBLE. fakeGit.HasDir took only the root and
// had no notion of a checked-out ref, so it answered identically before and
// after any Checkout -- a fixture that could not represent the bug. It now
// keys off the current ref.
//
// ⚠️ DriftOnly IS SET, AND IT HAS TO BE NOW. Rotation used to run on every
// non-drift pass; it now runs only on the daily/drift pass (the rotation
// fix elsewhere in this change), so a test that wants to exercise
// runRotation at all has to ask for a drift pass -- the "no new commits"
// framing below no longer matters on its own, since a drift pass skips the
// commit loop unconditionally rather than because there was nothing to do.
func TestRotationTestsTheTreeItIsAboutToRotate(t *testing.T) {
	const head = "headsha1"
	const last = "lastsha0"

	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: nil, // a drift pass never runs the commit loop anyway
		DirsAtRef: map[string][]string{
			// The tree before any checkout has NO credentials root...
			"": {"projects/recipes"},
			// ...but the tree at `last`, which rotation is about to use, does.
			last: {"credentials"},
		},
	}
	tofu := &fakeTofu{PlanDetailedChanged: false}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	deps.Cfg.DriftOnly = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, last)

	// Asking before the checkout sees the "" tree, finds no credentials
	// root, and skips -- silently stalling rotation. Asking after sees the
	// tree at `last`, finds it, and proceeds to plan.
	if len(tofu.appliedDirs()) == 0 && result.failure == "" {
		// Rotation may legitimately no-op (PlanDetailedChanged is false),
		// but it must have got far enough to RUN tofu against the root.
		if !git.checkedOut(last) {
			t.Fatal("rotation never checked out the last applied commit")
		}
	}
	if !git.checkedOut(last) {
		t.Errorf("rotation did not check out %s before testing for its credentials root", last)
	}
}
