package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// This file drives runApplyPass and asserts on d.Stderr directly, the way
// AGENTS.md's own rule requires for a batched job: "report the counter that
// MOVES" -- a pass that hangs mid-`tofu apply` and one that did nothing
// silently must not look the same in a pod log. logf (apply_cmd.go) is the
// mechanism; these are the tests that would fail if any of its nine call
// sites were deleted or misworded.

// TestANarratedPassNamesTheRootAndTheCommitItApplied drives the same
// matching-digest fixture as TestTheDigestGateAppliesOnlyWhatWasApproved
// (apply_digest_gate_test.go) and checks that the three lines a normal apply
// produces name the actual root and commit, not just that SOMETHING was
// printed.
func TestANarratedPassNamesTheRootAndTheCommitItApplied(t *testing.T) {
	const sha = "commitsha2"
	const head = "commitsha2"

	deps, fl, _, _ := gateDeps(t, sha, head)
	fl.put("digests/"+head+"/"+gateSlug+".digest", []byte(ourDigest(t)))

	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- the digest matches", result.failure)
	}

	out := stderr.String()
	if !strings.Contains(out, "considering "+sha) {
		t.Errorf("stderr = %q, want a line naming the commit under consideration", out)
	}
	if !strings.Contains(out, "applying "+gateRoot+" at "+sha) {
		t.Errorf("stderr = %q, want a line naming the root being applied and the commit sha", out)
	}
	if !strings.Contains(out, "plan for "+gateRoot+" matches the one approved at "+head) {
		t.Errorf("stderr = %q, want the plan-match line naming the root and the approved head sha", out)
	}
}

// TestALockContendedPassSaysSoRatherThanExitingSilently is the load-bearing
// test in this file. §2 item 7 makes lock contention a silent, non-failure
// stop -- no failed/<sha>, no alert text, HEAD unmoved -- which is exactly
// the shape AGENTS.md's rule warns about: "cannot be told apart from a hung
// one" unless something says so on the one channel that is not swallowed.
// This drives the same fixture as TestLockContentionFilesNothingAndAdvances-
// Nothing (apply_lock_test.go) and additionally asserts stderr names it.
func TestALockContendedPassSaysSoRatherThanExitingSilently(t *testing.T) {
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

	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)
	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- lock contention is not a failure", result.failure)
	}

	out := stderr.String()
	if !strings.Contains(out, "state lock held elsewhere") {
		t.Fatalf("stderr = %q, want it to say the state lock is held elsewhere -- a contended pass must not be silent", out)
	}
}

// TestANoopCommitNarratesWhyHeadAdvanced drives a commit that touches no
// root -- repo.TouchedRoots returns nothing for a change to a file outside
// every known root -- and checks the pass says why HEAD moved without
// applying anything, rather than advancing silently.
func TestANoopCommitNarratesWhyHeadAdvanced(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{
			sha: {"README.md"},
		},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(string) bool { return true },
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

	out := stderr.String()
	if !strings.Contains(out, "noop: "+sha+" touches no root") {
		t.Errorf("stderr = %q, want the noop line naming the commit", out)
	}
}

// TestADriftPassNarratesRotationAndPerRootDriftPlanning drives the daily
// (drift) pass fixture from apply_daily_pass_test.go and checks both halves
// narrate: rotation re-planning credentials, and drift planning each root it
// walks.
func TestADriftPassNarratesRotationAndPerRootDriftPlanning(t *testing.T) {
	const head = "headsha1"

	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, _, _ := dailyPassDeps(t, true, tofu)
	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, head)

	out := stderr.String()
	if !strings.Contains(out, "rotation: re-planning credentials at "+head) {
		t.Errorf("stderr = %q, want the rotation re-planning line", out)
	}
	if !strings.Contains(out, "drift: planning platform at "+head) {
		t.Errorf("stderr = %q, want the per-root drift planning line for platform", out)
	}
}

// TestNarrationNeverCarriesASecret is the negative half of the security
// constraint: narration must name roots and shas only, never credential
// material. It plants a recognisable sentinel as the GOOGLE_CREDENTIALS
// value every root's tofu run reads (buildBaseEnv / credCache.googleCreds,
// apply_cmd.go) and drives a full narrated apply, then checks the sentinel
// never reaches stderr.
//
// This assertion was mutation-tested: temporarily logging the env slice
// built by applyOneRoot made it fail with the sentinel printed verbatim,
// confirming the check can catch a real leak before it shipped.
func TestNarrationNeverCarriesASecret(t *testing.T) {
	const sha = "commitsha2"
	const head = "commitsha2"
	const sentinel = "SENTINEL-TOKEN-VALUE"

	deps, fl, _, _ := gateDeps(t, sha, head)
	fl.put("digests/"+head+"/"+gateSlug+".digest", []byte(ourDigest(t)))

	// Overwrite the credential mirror's google-creds field with a value that
	// would be unmistakable in a log line, the same file buildTestDeps
	// planted "fake-google-creds" into.
	credPath := filepath.Join(deps.Dir.Root, itemGCPApply, fieldGCPCredentials)
	if err := os.WriteFile(credPath, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("planting sentinel credential: %v", err)
	}

	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, head)

	if strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("narration leaked the credential mirror's own sentinel value onto stderr:\n%s", stderr.String())
	}
}

// TestNarrationStaysOffTheStdoutPayload checks the other side of the §2
// item 7-shaped divide this file's helper documents: cmdApply's stdout is
// exactly result.notifyText and nothing else (apply_cmd.go:
// `fmt.Fprintln(stdout, result.notifyText)`), so narration reaching that
// string would contaminate the one machine-readable channel this binary
// produces. Stderr, meanwhile, must carry the narration -- so this also
// confirms the two channels are not merely both non-empty, but actually
// separated.
func TestNarrationStaysOffTheStdoutPayload(t *testing.T) {
	const sha = "commitsha2"
	const head = "commitsha2"

	deps, fl, _, _ := gateDeps(t, sha, head)
	fl.put("digests/"+head+"/"+gateSlug+".digest", []byte(ourDigest(t)))

	var stderr bytes.Buffer
	deps.Stderr = &stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- the digest matches", result.failure)
	}

	const want = "platform applier: applied=1 noop=0 last=" + head
	if result.notifyText != want {
		t.Fatalf("stdout payload = %q, want %q", result.notifyText, want)
	}
	for _, frag := range []string{"considering", "applying", "[15:"} {
		if strings.Contains(result.notifyText, frag) {
			t.Errorf("stdout payload = %q, contains narration fragment %q -- narration leaked onto the notify text", result.notifyText, frag)
		}
	}

	if !strings.Contains(stderr.String(), "considering "+sha) {
		t.Errorf("stderr = %q, want narration on stderr while stdout carries none of it", stderr.String())
	}
}
