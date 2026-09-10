package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file closes the defect docs/work-items.md recorded under "The kind
// layer classifies directories the pass never executes": runCommitLoop
// derived tofu work from repo.TouchedRoots, which never names clusters/<name>
// or hosts/<name> even though repo.KindOf classifies both as KindTofu. A
// commit touching only clusters/beta/main.tf was recorded as a noop and HEAD
// advanced past it, silently.

// seedPlainLockfile gives a root a committed lockfile that declares only the
// github provider, the same shape apply_pass_test.go's seedRootLockfiles
// uses for "platform" -- rootDeclaresCloudflare (apply_cmd.go) reads this
// file and refuses outright if it is missing, and clusters/<name> and
// hosts/<name> are not part of that shared fixture.
func seedPlainLockfile(t *testing.T, workdir, root string) {
	t.Helper()
	dir := filepath.Join(workdir, root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seeding %s: %v", root, err)
	}
	const github = "provider \"registry.opentofu.org/integrations/github\" {\n  version = \"6.0.0\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), []byte(github), 0o644); err != nil {
		t.Fatalf("seeding %s lockfile: %v", root, err)
	}
}

// TestAClustersRootIsPlannedAndApplied drives a commit touching only
// clusters/beta/main.tf and requires it to be planned and applied, not
// nooped. THIS IS THE REPRODUCTION: before the fix, repo.TouchedRoots knows
// nothing about clusters/<name>, so this commit's touched-root set comes
// back empty and it is recorded as a noop with HEAD advanced past it.
func TestAClustersRootIsPlannedAndApplied(t *testing.T) {
	const sha = "commitsha-clusters"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"clusters/beta/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "clusters/beta" },
	}
	// A plan with no resource changes needs no filed digest (applyOneRoot's
	// "nothing to gate" branch), which keeps this fixture about the root
	// derivation and not the digest gate.
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	seedPlainLockfile(t, deps.Cfg.Workdir, "clusters/beta")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 1 || !strings.Contains(got[0], "clusters/beta") {
		t.Fatalf("tofu applied %v, want exactly one apply of clusters/beta -- the root was silently skipped", got)
	}
	// A noop record never calls tofu at all (runCommitLoop's noop branch
	// returns before applyOneRoot is ever reached), so the appliedDirs
	// assertion above already proves this was not filed as one; this checks
	// the ledger record itself carries a real per-root summary too.
	body, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatalf("applied/%s was not written at all", sha)
	}
	if !strings.Contains(string(body), "clusters/beta") {
		t.Fatalf("applied/%s = %s, want it to name clusters/beta, not the bare noop record", sha, body)
	}
}

// TestAHostsRootIsPlannedAndApplied is TestAClustersRootIsPlannedAndApplied's
// sibling for hosts/<name>, the other KindTofu directory repo.TouchedRoots
// never named.
func TestAHostsRootIsPlannedAndApplied(t *testing.T) {
	const sha = "commitsha-hosts"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"hosts/dev-beta/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "hosts/dev-beta" },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	seedPlainLockfile(t, deps.Cfg.Workdir, "hosts/dev-beta")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 1 || !strings.Contains(got[0], "hosts/dev-beta") {
		t.Fatalf("tofu applied %v, want exactly one apply of hosts/dev-beta -- the root was silently skipped", got)
	}
	body, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatalf("applied/%s was not written at all", sha)
	}
	if !strings.Contains(string(body), "hosts/dev-beta") {
		t.Fatalf("applied/%s = %s, want it to name hosts/dev-beta, not the bare noop record", sha, body)
	}
}

// TestPlatformAndClustersApplyBothInKindThenPathOrder drives a commit
// touching platform/ and clusters/beta/ together and requires both to
// apply, in repo.TouchedUnits' fixed order: both are KindTofu, so the tie is
// broken by path -- "clusters/beta" sorts before "platform".
func TestPlatformAndClustersApplyBothInKindThenPathOrder(t *testing.T) {
	const sha = "commitsha-both"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"platform/main.tf", "clusters/beta/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn: func(root string) bool {
			return root == "platform" || root == "clusters/beta"
		},
	}
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	seedPlainLockfile(t, deps.Cfg.Workdir, "clusters/beta")
	// platform's lockfile is seeded by buildTestDeps -> seedRootLockfiles.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	got := tofu.appliedDirs()
	if len(got) != 2 {
		t.Fatalf("tofu applied %v, want exactly two applies (clusters/beta and platform)", got)
	}
	if !strings.Contains(got[0], "clusters/beta") || !strings.Contains(got[1], "platform") {
		t.Fatalf("tofu applied %v in that order, want clusters/beta before platform (kind ties break on path)", got)
	}
	body, ok := fl.get("applied/" + sha)
	if !ok {
		t.Fatalf("applied/%s was not written at all", sha)
	}
	if !strings.Contains(string(body), "clusters/beta") || !strings.Contains(string(body), "\"platform\"") {
		t.Fatalf("applied/%s = %s, want it to name both roots", sha, body)
	}
}

// TestADocsOnlyCommitIsStillNoopWithTofuUnits pins the regression this
// change must not cause: a commit outside every known unit is still filed as
// a noop, now that the tofu half is derived from repo.TouchedUnits instead
// of repo.TouchedRoots.
func TestADocsOnlyCommitIsStillNoopWithTofuUnits(t *testing.T) {
	const sha = "commitsha-docsonly"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"docs/README.md"},
		},
		TreeTofuUnitsByCommit: map[string][]string{sha: nil},
		HasDirFn:              func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)
	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if !strings.Contains(stderr.String(), "noop: "+sha+" touches no root") {
		t.Fatalf("stderr = %q, want the noop line naming the commit", stderr.String())
	}
}

// TestASharedInputPlansEveryTofuUnitInTheTree drives a commit touching
// modules/ -- a shared input -- with a tree that contains one of every
// credentials/tofu kind, and requires every one of them to be planned and
// applied, in kind-then-path order.
func TestASharedInputPlansEveryTofuUnitInTheTree(t *testing.T) {
	const sha = "commitsha-shared"
	const head = sha

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"modules/vpc/main.tf"},
		},
		TreeTofuUnitsByCommit: map[string][]string{
			sha: {"clusters/beta", "hosts/dev-beta", "platform", "projects/recipes"},
		},
		HasDirFn: func(root string) bool {
			switch root {
			case "clusters/beta", "hosts/dev-beta", "platform", "projects/recipes":
				return true
			}
			return false
		},
	}
	tofu := &fakeTofu{PlanDetailedChanged: true, ShowJSONBytes: noopPlanJSON}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	seedPlainLockfile(t, deps.Cfg.Workdir, "clusters/beta")
	seedPlainLockfile(t, deps.Cfg.Workdir, "hosts/dev-beta")
	// platform and projects/recipes are seeded by buildTestDeps already.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	got := tofu.appliedDirs()
	wantOrder := []string{"clusters/beta", "hosts/dev-beta", "platform", "projects/recipes"}
	if len(got) != len(wantOrder) {
		t.Fatalf("tofu applied %v, want all four units in the tree", got)
	}
	for i, want := range wantOrder {
		if !strings.Contains(got[i], want) {
			t.Fatalf("tofu applied %v, want %v in that order at index %d", got, wantOrder, i)
		}
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Fatalf("applied/%s was not written", sha)
	}
}
