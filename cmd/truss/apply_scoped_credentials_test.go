package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/plan"
)

// ⚠️ ROOT CREDENTIALS MUST NOT GO WHERE THEY CANNOT BE USED.
//
// Every root that was not `credentials` used to be handed cf-infra-admin,
// which carries account-wide R2 write, account-wide Access "Apps and Policies"
// AND "Organizations, Identity Providers, and Groups" write, and zone DNS
// write. `platform/` does not use Cloudflare at all -- its committed lockfile
// declares only the github provider, verified against the real repo -- and it
// was handed that token on every apply and every drift plan.
//
// The owner's rule: root credentials exist to MINT scoped per-project
// credentials, not to be given to everything. This is the first step of that
// and the only one needing nothing new to exist.
//
// The two tests below pull in opposite directions on purpose. Over-tightening
// here is an outage -- a root silently stripped of a credential it needs fails
// later, inside a provider, with a message about something else.

// credEnvFor drives one commit touching one root and returns the environment
// the tofu runner was built with. The factory is the seam: applyOneRoot builds
// the env and hands it to NewTofu, so capturing it there is reading exactly
// what the tofu process would have received.
func credEnvFor(t *testing.T, root string) []string {
	t.Helper()
	const sha = "commitsha2"
	const head = "commitsha2"

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {root + "/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(r string) bool { return r == root },
	}
	tofu := &fakeTofu{PlanDetailedChanged: true}

	var captured []string
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner {
		captured = env
		return tofu
	})

	digest, err := plan.Digest(changingPlanJSON)
	if err != nil {
		t.Fatalf("plan.Digest: %v", err)
	}
	fl.put("digests/"+head+"/"+strings.ReplaceAll(root, "/", "-")+".digest", []byte(digest))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, head); result.failure != "" {
		t.Fatalf("pass over %s failed: %s", root, result.failure)
	}
	if captured == nil {
		t.Fatalf("no tofu runner was built for %s, so nothing was asserted", root)
	}
	return captured
}

func hasEnv(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

func TestARootThatDoesNotUseCloudflareIsNotGivenTheCloudflareToken(t *testing.T) {
	env := credEnvFor(t, "platform")
	if hasEnv(env, "CLOUDFLARE_API_TOKEN") {
		t.Fatal("platform/ declares no Cloudflare provider but was handed CLOUDFLARE_API_TOKEN")
	}
	// It must still get what it DOES need, or this "fix" is just a break.
	if !hasEnv(env, "GOOGLE_CREDENTIALS") {
		t.Fatal("platform/ lost GOOGLE_CREDENTIALS, which it does need")
	}
}

func TestARootThatUsesCloudflareStillGetsItsToken(t *testing.T) {
	env := credEnvFor(t, "projects/recipes")
	if !hasEnv(env, "CLOUDFLARE_API_TOKEN") {
		t.Fatal("projects/recipes declares the Cloudflare provider but was given no CLOUDFLARE_API_TOKEN")
	}
}
