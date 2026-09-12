package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/plan"
)

// TestAStopDuringAUnitLetsThatUnitFinishAndStartsNoOther is graceful stop's
// central claim, made deterministic with no real signal at all: deps.Stop
// (apply_cmd.go) is a seam, so the test flips it from INSIDE the first
// commit's own tofu apply -- fakeTofu's OnApply hook -- which is the only
// moment "does a stop let the in-flight unit finish" is actually about.
//
// Two commits, one root each. If the stop check fired mid-unit instead of
// between units, the first commit's apply would never be recorded. If it
// did not fire at all, the second commit would apply too.
func TestAStopDuringAUnitLetsThatUnitFinishAndStartsNoOther(t *testing.T) {
	const sha1, sha2 = "commitone", "committwo"
	yes := true
	webFlow := "web-flow"

	forgeFake := &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		PRNumbers:        map[string][]int{sha1: {1}, sha2: {2}},
		PR: map[int]gates.PullRequest{
			1: {Number: 1, Merged: true, MergeCommitSHA: sha1, HeadSHA: sha1},
			2: {Number: 2, Merged: true, MergeCommitSHA: sha2, HeadSHA: sha2},
		},
		ReviewsResult: map[int][]gates.Review{
			1: {{User: "alice", State: "APPROVED", CommitID: sha1}},
			2: {{User: "alice", State: "APPROVED", CommitID: sha2}},
		},
		CommitResult: map[string]gates.Commit{
			sha1: {SHA: sha1, Verified: &yes, CommitterLogin: &webFlow},
			sha2: {SHA: sha2, Verified: &yes, CommitterLogin: &webFlow},
		},
	}

	git := &fakeGit{
		CommitsList: []string{sha1, sha2},
		ChangedByCommit: map[string][]string{
			sha1: {"platform/main.tf"},
			sha2: {"projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha1: nil, sha2: nil},
		HasDirFn:          func(root string) bool { return true },
	}

	var stopped atomic.Bool
	tofu := &fakeTofu{
		ShowJSONBytes: changingPlanJSON,
		OnApply: func(dir string) {
			stopped.Store(true)
		},
	}

	deps, fl, ft := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })
	deps.Stop = stopped.Load

	approved, err := plan.Digest(changingPlanJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	fl.put("digests/"+sha1+"/platform.digest", []byte(approved))
	fl.put("digests/"+sha2+"/projects-recipes.digest", []byte(approved))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "startsha")

	if got := tofu.appliedDirs(); len(got) != 1 || !strings.Contains(got[0], "platform") {
		t.Fatalf("tofu applied %v, want exactly one apply, for platform (commit one's root)", got)
	}
	if git.checkedOut(sha2) {
		t.Errorf("commit two (%s) was checked out, want the stop to prevent it from ever starting", sha2)
	}
	if result.failure != "" {
		t.Errorf("result.failure = %q, want empty: a graceful stop is not a failure", result.failure)
	}
	if _, ok := fl.get("applied/" + sha1); !ok {
		t.Errorf("applied/%s is missing, want the finished commit recorded despite the stop", sha1)
	}
	if _, ok := fl.get("applied/" + sha2); ok {
		t.Errorf("applied/%s is present, want the never-started commit left unrecorded", sha2)
	}
	if _, ok := fl.get("heartbeat"); !ok {
		t.Fatalf("no heartbeat was written: a graceful stop must still reach the pass's tail")
	}
	if ft.lastText() == "" {
		t.Fatalf("no message reached Telegram: a graceful stop must still alert")
	}
}

// TestAStopDoesNotFireInsideARootsApply is the mirror image: OnApply never
// sees the flag flip DURING its own call, because stopping() is polled
// before Apply is reached, never during it. This guards against a future
// change accidentally moving the check inside applyOneRoot.
func TestAStopDoesNotFireInsideARootsApply(t *testing.T) {
	const sha = "onlycommit"
	yes := true
	webFlow := "web-flow"

	forgeFake := &fakeForge{
		ProtectionResult: compliantGatesProtection(),
		PRNumbers:        map[string][]int{sha: {1}},
		PR: map[int]gates.PullRequest{
			1: {Number: 1, Merged: true, MergeCommitSHA: sha, HeadSHA: sha},
		},
		ReviewsResult: map[int][]gates.Review{
			1: {{User: "alice", State: "APPROVED", CommitID: sha}},
		},
		CommitResult: map[string]gates.Commit{
			sha: {SHA: sha, Verified: &yes, CommitterLogin: &webFlow},
		},
	}
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {"platform/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return true },
	}

	var stopped atomic.Bool
	tofu := &fakeTofu{ShowJSONBytes: changingPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })
	deps.Stop = stopped.Load
	stopped.Store(true) // already stopping before the pass even starts

	approved, err := plan.Digest(changingPlanJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	fl.put("digests/"+sha+"/platform.digest", []byte(approved))

	result := runApplyPass(context.Background(), deps, "startsha")

	if len(tofu.appliedDirs()) != 0 {
		t.Fatalf("tofu applied %v, want none: a stop already pending before the pass starts must apply nothing", tofu.appliedDirs())
	}
	if result.failure != "" {
		t.Errorf("result.failure = %q, want empty", result.failure)
	}
	if _, ok := fl.get("heartbeat"); !ok {
		t.Fatalf("no heartbeat was written")
	}
}
