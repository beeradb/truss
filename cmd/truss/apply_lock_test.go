package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// TestLockContentionFilesNothingAndAdvancesNothing drives a commit whose
// one root's `tofu init` reports the state lock held elsewhere. §2 item 7:
// "A busy state lock is contention, not a fault... the pass ends with no
// failed/<sha>, no failure alert, HEAD unmoved, exit 0." This is the one
// place besides a config refusal where the pass can stop without treating
// itself as having failed.
func TestLockContentionFilesNothingAndAdvancesNothing(t *testing.T) {
	const sha = "commitsha2"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{
			sha: nil,
		},
		HasDirFn: func(root string) bool { return root == "projects/recipes" },
	}
	newTofu := func(env []string) tofuRunner {
		return &fakeTofu{InitErr: plan.ErrLockBusy}
	}

	deps, fl, ft := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- lock contention is not a failure", result.failure)
	}

	if _, ok := fl.get("failed/" + sha); ok {
		t.Fatalf("failed/%s was written, want nothing filed for a contended commit", sha)
	}
	if headBytes, ok := fl.get("head"); ok {
		// runApplyPass takes `last` as a parameter and never reads the
		// ledger's head key itself; the only way "head" gets written is
		// AdvanceHead, which a contended commit must never reach.
		t.Fatalf("head was advanced to %q, want it left unmoved (nothing written)", headBytes)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Fatalf("applied/%s was written, want nothing recorded for a contended commit", sha)
	}

	// The pass still finishes, still writes a heartbeat and still alerts
	// (§2 item 8) -- contention ends the COMMIT LOOP early, not the pass.
	if _, ok := fl.get("heartbeat"); !ok {
		t.Fatalf("no heartbeat was written even though the pass otherwise completed")
	}
	if ft.lastText() == "" {
		t.Fatalf("no message reached the fake Telegram server")
	}
}

// TestALockHeldFarTooLongIsAFailureNotContention. A lock nobody released is
// not a second applier working: the GCS backend's lock object has no lease
// and no expiry, so a pod killed mid-apply leaves one for ever. Every later
// pass then reads contention, ends early, exits 0 and alerts nobody --
// measured at 16 hours on 2026-09-07 (platform applier/force-unlock). The
// deferral is right for a live holder and wrong for a corpse, and only the
// lock's age tells them apart.
//
// The failure must name the root and the lock ID, because those two are
// exactly what `tofu force-unlock` is run with.
func TestALockHeldFarTooLongIsAFailureNotContention(t *testing.T) {
	const sha = "commitsha2"
	const head = "headsha1"
	const lockID = "ff3fa170-d20d-971b-e575-af5503a795ee"

	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{
			sha: nil,
		},
		HasDirFn: func(root string) bool { return root == "projects/recipes" },
	}
	// buildTestDeps pins the pass's clock at 2026-09-07 12:00 UTC, so this
	// is the real incident: the lock taken at 05:56 that morning by a pod
	// that no longer existed, six hours in and still being deferred.
	stale := &plan.LockBusyError{Info: plan.LockInfo{
		ID:      lockID,
		Who:     "applier@applier-truss-29321130-abcde",
		Created: time.Date(2026, 9, 7, 5, 56, 12, 0, time.UTC),
	}}
	newTofu := func(env []string) tofuRunner {
		return &fakeTofu{InitErr: stale}
	}

	deps, fl, ft := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure == "" {
		t.Fatal("result.failure is empty; a lock held for 16 hours must fail the pass, not defer it again")
	}
	if !strings.Contains(result.failure, "projects/recipes") {
		t.Errorf("failure %q does not name the root force-unlock needs", result.failure)
	}
	if !strings.Contains(result.failure, lockID) {
		t.Errorf("failure %q does not name the lock ID force-unlock needs", result.failure)
	}

	if _, ok := fl.get("failed/" + sha); !ok {
		t.Errorf("failed/%s was not written; a failure nobody records is one nobody can read back", sha)
	}
	if headBytes, ok := fl.get("head"); ok {
		t.Errorf("head was advanced to %q; the commit never applied", headBytes)
	}
	if ft.lastText() == "" {
		t.Error("no message reached the fake Telegram server; this is the alert the 16 hours went without")
	}
}

// TestALockTakenMinutesAgoIsStillContention is the other half, and the one
// that makes the threshold mean something. Without it, lockLeakAfter could be
// zero -- every contention a failure, the deferral gone -- and the suite would
// stay green, because the test above only ever presents a lock that is
// already hours old and the sentinel-only test carries no age at all.
//
// A second applier mid-apply is exactly what deferring is for: file nothing,
// move nothing, and let the pass end quietly.
func TestALockTakenMinutesAgoIsStillContention(t *testing.T) {
	const sha = "commitsha2"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{
			sha: nil,
		},
		HasDirFn: func(root string) bool { return root == "projects/recipes" },
	}
	// One minute before the clock buildTestDeps pins: a live holder.
	fresh := &plan.LockBusyError{Info: plan.LockInfo{
		ID:      "ff3fa170-d20d-971b-e575-af5503a795ee",
		Who:     "applier@applier-truss-29321131-fghij",
		Created: time.Date(2026, 9, 7, 11, 59, 0, 0, time.UTC),
	}}
	newTofu := func(env []string) tofuRunner {
		return &fakeTofu{InitErr: fresh}
	}

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a lock a minute old is another applier working", result.failure)
	}
	if _, ok := fl.get("failed/" + sha); ok {
		t.Errorf("failed/%s was written for a lock held one minute", sha)
	}
	if headBytes, ok := fl.get("head"); ok {
		t.Errorf("head was advanced to %q while a live holder had the lock", headBytes)
	}
}
