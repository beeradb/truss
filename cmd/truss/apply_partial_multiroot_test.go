package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// A COMMIT TOUCHING TWO ROOTS HAS NO PER-ROOT PROGRESS RECORD, SO A FAILURE
// IN THE SECOND ROOT WEDGES THE QUEUE PERMANENTLY.
//
// applyOneRoot is called per root in a loop (apply_cmd.go:645), and each call
// runs init, plan, digest-compare and APPLY before the next root is planned.
// applied/<sha> and AdvanceHead are written only after every root in the
// commit has succeeded. So when root 2 fails, root 1 is already applied to
// real infrastructure and HEAD still points at the previous commit.
//
// The next pass re-derives the same commit and re-plans root 1 -- against
// infrastructure that now already has root 1's changes. That plan is a no-op,
// Canonical drops no-ops, and its digest is the digest of `[]`. CI's filed
// digest is of the plan that CHANGED something. They cannot match, and the
// mismatch is reported as "the world moved between review and apply" -- a
// refusal whose stated reason is not what actually happened.
//
// Nothing recovers from this. Every subsequent pass repeats it identically,
// so the queue stops at this commit until somebody advances the watermark by
// hand. The shared-input case is the dangerous one: touching modules/,
// providers.allow or .opentofu-version plans EVERY root (repo.TouchedRoots),
// so a provider bump is always a multi-root commit.
//
// FIXED 2026-09-09 in applyOneRoot: a plan that changes nothing is no longer
// digest-gated, because there is nothing to gate -- see the comment there.
// This test is the reproduction that found it, kept as the guard: it fails
// again the moment an empty plan is refused for not matching a digest.

// dirKeyedTofu answers per root directory and per pass, which the shared
// fakeTofu cannot do: reproducing this needs root 1 to apply cleanly and root
// 2 to fail on pass one, then root 1's plan to have become a no-op on pass
// two. Keyed on the dir each call is given, since that is the only thing
// identifying the root to the runner.
type dirKeyedTofu struct {
	mu      sync.Mutex
	pass    int
	applies []string

	// showJSON answers ShowJSON for (pass, dir); applyErr answers Apply.
	showJSON func(pass int, dir string) []byte
	applyErr func(pass int, dir string) error
}

func (f *dirKeyedTofu) Init(context.Context, string) error                { return nil }
func (f *dirKeyedTofu) Plan(context.Context, string, string) error        { return nil }
func (f *dirKeyedTofu) ForceUnlock(context.Context, string, string) error { return nil }
func (f *dirKeyedTofu) PlanDetailed(context.Context, string) (bool, error) {
	return false, nil
}

func (f *dirKeyedTofu) Apply(_ context.Context, dir, _ string) error {
	f.mu.Lock()
	f.applies = append(f.applies, dir)
	pass := f.pass
	f.mu.Unlock()
	return f.applyErr(pass, dir)
}

func (f *dirKeyedTofu) ShowJSON(_ context.Context, dir, _ string) ([]byte, error) {
	f.mu.Lock()
	pass := f.pass
	f.mu.Unlock()
	return f.showJSON(pass, dir), nil
}

func (f *dirKeyedTofu) appliedDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applies...)
}

func (f *dirKeyedTofu) nextPass() {
	f.mu.Lock()
	f.pass++
	f.applies = nil
	f.mu.Unlock()
}

func TestAPartiallyAppliedMultiRootCommitWedgesTheQueue(t *testing.T) {
	const sha = "multirootsha"
	const head = "multirootsha"

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	// One commit, two roots. platform sorts before projects/*, so platform
	// is applied first and projects/recipes is the one that fails.
	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"platform/main.tf", "projects/recipes/main.tf"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn: func(root string) bool {
			return root == "platform" || root == "projects/recipes"
		},
	}

	tofu := &dirKeyedTofu{
		showJSON: func(pass int, dir string) []byte {
			// On the second pass platform has already been applied, so
			// re-planning it produces a no-op. projects/recipes never applied,
			// so its plan is unchanged.
			if pass > 0 && strings.Contains(dir, "platform") {
				return noopAfterApplyJSON
			}
			return changingPlanJSON
		},
		applyErr: func(pass int, dir string) error {
			// A transient failure on the second root, first pass only --
			// the kind of thing that resolves itself by the next pass.
			if pass == 0 && strings.Contains(dir, "projects/recipes") {
				return errors.New("cloud API returned 503")
			}
			return nil
		},
	}

	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })

	// CI filed a digest of the changing plan for both roots, at the head sha.
	approved, err := plan.Digest(changingPlanJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	fl.put("digests/"+head+"/platform.digest", []byte(approved))
	fl.put("digests/"+head+"/projects-recipes.digest", []byte(approved))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// PASS ONE: platform applies, projects/recipes fails.
	first := runApplyPass(ctx, deps, "startsha")
	if first.failure == "" {
		t.Fatal("pass one: want a failure from the second root, got none")
	}
	// platform must have been applied FIRST and for real -- that is the
	// state that wedged the queue. The fake records an attempt before
	// returning its error, so projects/recipes appears here too; what
	// matters is the order and that platform's apply returned nil.
	if got := tofu.appliedDirs(); len(got) != 2 || !strings.Contains(got[0], "platform") {
		t.Fatalf("pass one: tofu applied %v, want platform first and then the root that fails", got)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Error("pass one: applied/" + sha + " was written for a commit that did not finish")
	}

	// PASS TWO: the transient failure is gone. The commit should now finish.
	tofu.nextPass()
	second := runApplyPass(ctx, deps, "startsha")

	if second.failure != "" {
		t.Errorf("pass two: the transient failure cleared, but the commit still refuses:\n  %s", second.failure)
	}
	if got := tofu.appliedDirs(); len(got) == 0 {
		t.Error("pass two: tofu applied nothing -- the queue is wedged on an already-half-applied commit")
	} else if !strings.Contains(strings.Join(got, " "), "projects/recipes") {
		t.Errorf("pass two: tofu applied %v, but projects/recipes -- the root that failed -- was never reached", got)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Errorf("pass two: applied/%s still not written; the commit can never complete", sha)
	}
}
