package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestEveryPassCleansTheTreeNotJustTheFirst is the loop-mode property
// CleanTree exists for: cleaning is a COUNT, not a bool, on fakeGit
// specifically so this test can tell "cleaned once at startup" apart from
// "cleaned every pass" -- a test that only asserted the call happened at
// least once would pass against either.
func TestEveryPassCleansTheTreeNotJustTheFirst(t *testing.T) {
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	git := &fakeGit{}
	tofu := &fakeTofu{}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runApplyPass(ctx, deps, "startsha")
	if got := git.cleaned(); got != 1 {
		t.Fatalf("cleaned() = %d after one pass, want 1", got)
	}

	runApplyPass(ctx, deps, "startsha")
	if got := git.cleaned(); got != 2 {
		t.Fatalf("cleaned() = %d after two passes, want 2: CleanTree must run every pass, not just the first", got)
	}
}

// TestACleanTreeFailureIsARepoClassRefusalNotADirtyApply proves the
// failure path: a tree that will not clean must stop the pass before
// anything is planned, never fall through to applying from an unknown
// tree.
func TestACleanTreeFailureIsARepoClassRefusalNotADirtyApply(t *testing.T) {
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	git := &fakeGit{CleanTreeErr: errors.New("simulated: could not clean the working tree")}
	tofu := &fakeTofu{}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")

	if result.failure == "" {
		t.Fatalf("result.failure is empty, want a refusal naming the clean failure")
	}
	if len(tofu.appliedDirs()) != 0 {
		t.Errorf("tofu applied %v, want none: a tree that will not clean must apply nothing", tofu.appliedDirs())
	}
	if git.fetched() {
		t.Errorf("git was fetched, want no fetch after a failed clean")
	}
	if _, ok := fl.get("heartbeat"); !ok {
		t.Fatalf("no heartbeat was written")
	}
}
