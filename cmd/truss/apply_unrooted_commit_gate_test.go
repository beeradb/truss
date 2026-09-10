package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestACommitTouchingNoRootIsStillGated drives a commit that touches no
// OpenTofu root and that NO merged pull request produced, and requires the
// pass to refuse it.
//
// ⚠️ THIS IS A REPRODUCTION, AND IT IS RED UNTIL runCommitLoop IS REORDERED.
// runCommitLoop derives the touched roots first (apply_cmd.go:696) and, when
// the set is empty, records a noop and advances HEAD (apply_cmd.go:698-709)
// -- and both of those happen BEFORE checkCommitGate is ever called at line
// 711. So a commit that reaches main without an approval, without a merged
// PR, and without a forge-signed merge commit is filed as a noop and walked
// past, provided it happens not to touch a directory the tofu kinds claim.
//
// Today that costs nothing, because a commit touching no root changes
// nothing truss applies -- which is exactly why it has gone unnoticed. It
// stops being harmless the moment anything else reads this repository. A
// reconciler tracking a ref that the applier advances would apply the very
// commit truss filed as "nothing happened": truss's noop record is a
// statement that TRUSS did nothing, never a statement that nothing was done.
//
// The fixture uses a delivery path for that reason, but the defect is not
// about deliveries. Any path outside credentials/, platform/ and projects/*
// -- docs/, a README, a CI workflow -- takes the same route.
func TestACommitTouchingNoRootIsStillGated(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	// The forge answers that NO pull request produced this commit, which is
	// what a direct push to main looks like from the API. checkCommitGate
	// refuses that with "expected exactly one PR for <sha>, found 0" -- so
	// if the gate runs at all, this pass fails.
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			// ⚠️ A DOCS PATH, AND IT HAS TO BE. An earlier version of this
			// fixture used a delivery path, which stopped reproducing the
			// defect the moment render units existed: a delivery IS a unit,
			// so the noop-and-advance branch is not taken for it and the
			// test passed with the gate back in its old place. Measured by
			// mutation, 2026-09-10. The route the defect actually took is a
			// path no kind claims -- docs, a README, a CI workflow.
			sha: {"docs/README.md"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure == "" {
		t.Fatalf("result.failure = %q, want a refusal: a commit no merged PR "+
			"produced must not be walked past just because it touches no root", result.failure)
	}
	if !strings.Contains(result.failure, sha) {
		t.Errorf("result.failure = %q, want it to name the commit %s", result.failure, sha)
	}

	// A refusal that still advanced HEAD would be worse than no refusal: the
	// queue would report the failure once and never see the commit again.
	if got, ok := fl.get("head"); ok && strings.Contains(string(got), sha) {
		t.Errorf("head = %q, want it left at %s -- a refused commit must not advance HEAD", got, head)
	}
}
