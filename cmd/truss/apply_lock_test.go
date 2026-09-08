package main

import (
	"context"
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
