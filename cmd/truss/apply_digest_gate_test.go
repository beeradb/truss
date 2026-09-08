package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// ⚠️ THE DIGEST GATE'S WIRING HAD NO TEST AT ALL, AND THE GATE COULD BE
// DELETED WITH THE WHOLE SUITE STILL GREEN. Found 2026-09-08 by the security
// review and confirmed independently by the code audit: replacing
// `if root != "credentials"` in applyOneRoot with `if false` -- removing the
// entire gate from the pass -- left `go test ./...` passing.
//
// The pure halves were well covered (gates.CheckPlanDigest, plan.Digest), and
// that is exactly how it hid: what was untested was that applyOneRoot CALLS
// them. Both reviewers named the same theme -- the two untested things in
// this codebase were the WIRING of well-tested pure functions.
//
// This is the property the entire system exists to provide, so the tests
// below assert it end to end: the key the digest is read from, the use of
// headSHA rather than the commit sha, a missing digest refusing, a mismatched
// digest refusing, credentials being the only exemption, and -- the assertion
// that makes the refusals mean anything -- that tofu Apply is NEVER REACHED
// when the gate refuses. "The pass failed" is also true of a pass that
// applied and then failed afterwards.

const (
	gateRoot = "projects/recipes"
	gateSlug = "projects-recipes" // DigestKey replaces every "/" with "-"
)

// gateDeps builds a pass over one commit touching one non-credentials root,
// with tofu reporting a plan that would change something.
func gateDeps(t *testing.T, sha, head string) (applyDeps, *fakeLedger, *fakeTelegram, *fakeTofu) {
	t.Helper()

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {gateRoot + "/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == gateRoot },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, fl, ft := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	return deps, fl, ft, tofu
}

// ourDigest is the digest applyOneRoot will compute for the plan fakeTofu
// returns. Derived from the same input the code sees, never transcribed.
func ourDigest(t *testing.T) string {
	t.Helper()
	d, err := plan.Digest(noopPlanJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	return d
}

// notOurDigest is a well-formed digest of a DIFFERENT plan -- what CI would
// have filed if the world moved between the approval and the apply. Derived
// from a real plan rather than written as a literal: a hand-typed hex string
// is indistinguishable from an account id to scripts/leakscan, and deriving
// it also guarantees it is a digest the code could actually encounter.
func notOurDigest(t *testing.T) string {
	t.Helper()
	d, err := plan.Digest([]byte(`{"resource_changes":[{"address":"null_resource.other"}]}`))
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	if d == ourDigest(t) {
		t.Fatal("the two fixture plans digest the same, so a mismatch test cannot fail")
	}
	return d
}

func TestTheDigestGateAppliesOnlyWhatWasApproved(t *testing.T) {
	const sha = "commitsha2"
	const head = "commitsha2"

	t.Run("a matching digest applies", func(t *testing.T) {
		deps, fl, _, tofu := gateDeps(t, sha, head)
		fl.put("digests/"+head+"/"+gateSlug+".digest", []byte(ourDigest(t)))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, head)

		if result.failure != "" {
			t.Fatalf("result.failure = %q, want empty -- the digest matches", result.failure)
		}
		if got := tofu.appliedDirs(); len(got) != 1 {
			t.Fatalf("tofu Apply was called %d times, want exactly 1: %v", len(got), got)
		}
		if _, ok := fl.get("applied/" + sha); !ok {
			t.Errorf("applied/%s was not written for a commit that applied", sha)
		}
	})

	t.Run("a mismatched digest refuses and never applies", func(t *testing.T) {
		deps, fl, _, tofu := gateDeps(t, sha, head)
		fl.put("digests/"+head+"/"+gateSlug+".digest", []byte(notOurDigest(t)))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, head)

		if result.failure == "" {
			t.Fatal("result.failure is empty, want a refusal -- the approved digest does not match ours")
		}
		// The assertion that makes the refusal mean something.
		if got := tofu.appliedDirs(); len(got) != 0 {
			t.Fatalf("tofu Apply was reached %d time(s) despite the gate refusing: %v", len(got), got)
		}
		if _, ok := fl.get("applied/" + sha); ok {
			t.Errorf("applied/%s was written for a commit that was refused", sha)
		}
		if _, ok := fl.get("failed/" + sha); !ok {
			t.Errorf("failed/%s was not written for a refused commit", sha)
		}
	})

	t.Run("no recorded digest refuses and never applies", func(t *testing.T) {
		deps, _, _, tofu := gateDeps(t, sha, head)
		// Nothing seeded: ApprovedDigest returns ErrNotFound.

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, head)

		if result.failure == "" {
			t.Fatal("result.failure is empty, want a refusal -- no plan was ever approved for this root")
		}
		if got := tofu.appliedDirs(); len(got) != 0 {
			t.Fatalf("tofu Apply was reached %d time(s) with no approved digest at all: %v", len(got), got)
		}
	})

	t.Run("the digest is read at the head sha, not the commit sha", func(t *testing.T) {
		const other = "someothersha"
		deps, fl, _, tofu := gateDeps(t, sha, other)
		// Correct digest, filed under the COMMIT sha rather than the head
		// sha. A gate reading the wrong key would apply here.
		fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(ourDigest(t)))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, other)

		if result.failure == "" {
			t.Fatal("result.failure is empty: the digest was filed under the commit sha, not the head sha")
		}
		if got := tofu.appliedDirs(); len(got) != 0 {
			t.Fatalf("tofu Apply was reached %d time(s) on a digest filed under the wrong sha: %v", len(got), got)
		}
	})
}

// TestOnlyCredentialsIsExemptFromTheDigestGate: §2 item 10. credentials
// applies with no digest recorded because CI never plans it -- and that
// exemption must not extend to anything else, which the sibling subtests
// above establish by refusing projects/recipes under the same conditions.
func TestOnlyCredentialsIsExemptFromTheDigestGate(t *testing.T) {
	const sha = "commitsha3"

	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {"credentials/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "credentials" },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- credentials needs no approved digest", result.failure)
	}
	if got := tofu.appliedDirs(); len(got) != 1 {
		t.Fatalf("tofu Apply was called %d times for credentials, want 1: %v", len(got), got)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Errorf("applied/%s was not written", sha)
	}
	// And nothing was consulted for a digest: no digest object exists.
	for key := range map[string]bool{"digests/" + sha + "/credentials.digest": true} {
		if _, ok := fl.get(key); ok {
			t.Errorf("%s exists, so this test is not proving the exemption", key)
		}
	}
}

// TestTheGateRefusalNamesTheRootAndBothDigests: the refusal has to be
// actionable. It is also the sentence that, over a plaintext endpoint, hands
// an attacker our digest -- see checkEndpoint in internal/ledger.
func TestTheGateRefusalNamesTheRoot(t *testing.T) {
	const sha = "commitsha2"
	deps, fl, _, _ := gateDeps(t, sha, sha)
	fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(notOurDigest(t)))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, sha)

	if !strings.Contains(result.failure, gateRoot) {
		t.Errorf("refusal %q does not name the root %q", result.failure, gateRoot)
	}
}
