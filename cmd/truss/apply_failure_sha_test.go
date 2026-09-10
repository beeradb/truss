package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
)

// This file is the pass-level reproduction of the defect
// internal/notify's TestAFailureNamesTheCommitThatFailedNotTheLedgerPosition
// covers at the composer: the failure alert used to print the LEDGER
// POSITION under the words "FAILED at <sha>", so a refusal of one commit
// pointed a reader at the previous one.
//
// ⚠️ THE FIXTURE HAS TO ADVANCE THE QUEUE BEFORE IT FAILS, OR IT CANNOT
// TELL THE FIX FROM THE DEFECT. Most passes apply nothing, so the ledger
// position and the failing commit coincide and both the old code and the
// new produce the same string. That coincidence is where this bug lived
// for as long as it did.

// TestTheAlertNamesTheFailingCommitAndTheTreeItPlanned drives two commits:
// the first applies and advances HEAD, the second is refused because nobody
// approved it. The ledger then sits at the first while the failure belongs
// to the second.
func TestTheAlertNamesTheFailingCommitAndTheTreeItPlanned(t *testing.T) {
	const (
		first  = "commitsha-good"
		second = "commitsha-bad"
		head   = second
	)

	yes := true
	webFlow := "web-flow"
	forgeFake := &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		PRNumbers:        map[string][]int{first: {1}, second: {2}},
		PR: map[int]gates.PullRequest{
			1: {Number: 1, Merged: true, MergeCommitSHA: first, HeadSHA: head},
			2: {Number: 2, Merged: true, MergeCommitSHA: second, HeadSHA: head},
		},
		// ⚠️ PR 2 HAS NO APPROVING REVIEW, which is what refuses the second
		// commit. Chosen over a tofu failure because the commit gate runs
		// before any checkout, so PlannedSHA is legitimately empty here --
		// which is the case that must NOT print a "(planned at …)" clause.
		ReviewsResult: map[int][]gates.Review{
			1: {{User: "alice", State: "APPROVED", CommitID: head}},
			2: {},
		},
		CommitResult: map[string]gates.Commit{
			first:  {SHA: first, Verified: &yes, CommitterLogin: &webFlow},
			second: {SHA: second, Verified: &yes, CommitterLogin: &webFlow},
		},
	}

	git := &fakeGit{
		CommitsList: []string{first, second},
		ChangedByCommit: map[string][]string{
			first:  {"projects/recipes/main.tf"},
			second: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{first: nil, second: nil},
		HasDirFn:          func(root string) bool { return root == "projects/recipes" },
	}
	// A plan with no resource changes needs no filed digest, which keeps
	// this fixture about the alert rather than the digest gate.
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "base")

	if result.failure == "" {
		t.Fatalf("the pass reported no failure; the fixture is not exercising the refusal")
	}
	// The queue really did advance, which is what makes this fixture able to
	// tell the two shas apart at all.
	if _, ok := fl.get("applied/" + first); !ok {
		t.Fatalf("applied/%s was not written; the first commit did not apply, so the ledger position never moved", first)
	}
	if !strings.Contains(result.notifyText, "FAILED at "+second) {
		t.Fatalf("alert = %q, want it to name %s -- the commit that failed", result.notifyText, second)
	}
	if strings.Contains(result.notifyText, "FAILED at "+first) {
		t.Fatalf("alert = %q, names %s, which is the LEDGER POSITION and not where the failure was", result.notifyText, first)
	}
	// The commit gate refuses before anything is checked out, so there is no
	// planned tree to name and the clause must be absent rather than empty.
	if strings.Contains(result.notifyText, "planned at") {
		t.Fatalf("alert = %q, claims a tree was planned; the commit gate refused before any checkout", result.notifyText)
	}
}

// TestAFailureBelongingToNoCommitNamesNone is the other half. A branch
// protection refusal happens before the queue is read at all, so there is no
// commit it belongs to -- and printing a plausible one is what the reference
// bash did.
func TestAFailureBelongingToNoCommitNamesNone(t *testing.T) {
	forgeFake := &fakeForge{ProtectionResult: gates.Protection{}}
	git := &fakeGit{}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return &fakeTofu{} })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "base")

	if result.failure == "" {
		t.Fatalf("a protection fixture below the bar produced no failure")
	}
	if !strings.Contains(result.notifyText, "FAILED: ") {
		t.Fatalf("alert = %q, want it to name no commit at all", result.notifyText)
	}
	if strings.Contains(result.notifyText, "FAILED at ") {
		t.Fatalf("alert = %q, names a commit for a refusal that never read the queue", result.notifyText)
	}
}
