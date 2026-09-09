package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAnUnusableExpirySweepIsReportedAndDoesNotFailThePass pins the shape
// chosen on 2026-09-08 after both reviews: the sweep refuses to claim a
// clean bill it did not earn, so its problem must REACH somebody -- but it
// is not a failure, so it must not turn a clean pass red.
//
// ⚠️ IT USED TO SET failure, AND THAT WOULD HAVE MADE EVERY PRODUCTION PASS
// RED. Nothing seeds `expires` into Vault yet, so "lists N items but not one
// records an expiry" fires on every run: exit 1 plus a Telegram FAILED every
// five minutes, ~288 a day. An alert channel nobody reads is where a real
// digest-gate refusal goes to die, which is why both reviewers called it a
// security cost rather than noise.
//
// ⚠️ THIS IS NOW A DRIFT PASS, AND ORIGINALLY WAS NOT. The expiry sweep now
// runs only when DriftOnly is set -- the sweep is daily-only, because
// asking it every five minutes is what rate-limited the 1Password service
// account on 2026-09-07. A commit-loop pass, which this test used
// to drive, never reaches the sweep at all any more, so it cannot exercise
// this behaviour; a digest-gate refusal is also not available as "the
// unrelated failure reason" because drift never checks a digest. Branch
// protection failing stands in instead: the sweep sits after, and is
// unconditional on, the whole gate_ok if/else block, so a gate failure and
// an unusable sweep coexist in the same alert without either explaining the
// other -- which is exactly the property this test is pinning.
func TestAnUnusableExpirySweepIsReportedAndDoesNotFailThePass(t *testing.T) {
	forgeFake := &fakeForge{
		// Zero-value gates.Protection: every pointer nil, which
		// CheckProtection refuses -- the pass fails here, before it could
		// ever reach a commit or a digest.
	}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, ft := buildTestDeps(t, forgeFake, git, newTofu)
	deps.Cfg.DriftOnly = true
	// The vault is production-shaped (items, none with an expires), so the
	// sweep genuinely cannot report -- see productionShapedVault.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	// The pass refuses at the branch-protection gate, and that is the only
	// reason it fails: the sweep must not have contributed.
	if !strings.Contains(result.failure, "branch protection") {
		t.Fatalf("result.failure = %q, want a branch-protection refusal", result.failure)
	}
	if strings.Contains(result.failure, "expiry") {
		t.Errorf("result.failure names the expiry sweep, which must not fail the pass: %q", result.failure)
	}

	text := ft.lastText()
	if text == "" {
		t.Fatal("no message reached the fake Telegram server")
	}
	if !strings.Contains(text, "EXPIRY NOT CHECKED") {
		t.Errorf("the alert does not report the unusable sweep, so nobody learns of it: %q", text)
	}
	if !strings.Contains(text, "not one records an expiry") {
		t.Errorf("the alert does not say WHY the sweep could not report: %q", text)
	}
}

// TestACleanPassStaysCleanWithAnUnusableSweep is the half that matters most:
// with the vault shaped as production is today, a pass that has nothing else
// wrong with it exits 0 and does not send FAILED.
//
// ⚠️ ALSO NOW A DRIFT PASS -- see the sibling test above for why the sweep
// can no longer be reached from a commit-loop pass. A clean drift pass needs
// no digest at all (drift never checks one): compliant branch protection, a
// successful clone, and rotation/drift each skipping cleanly (no
// credentials or platform/projects root at HEAD, via HasDirFn returning
// false) is a clean pass on its own.
func TestACleanPassStaysCleanWithAnUnusableSweep(t *testing.T) {
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	tofu := &fakeTofu{}
	deps, _, ft := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	deps.Cfg.DriftOnly = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- an unusable expiry sweep is not a failed pass", result.failure)
	}
	text := ft.lastText()
	if strings.Contains(text, "FAILED") {
		t.Errorf("the alert leads with FAILED on an otherwise clean pass: %q", text)
	}
	if !strings.Contains(text, "EXPIRY NOT CHECKED") {
		t.Errorf("the sweep's problem was swallowed entirely: %q", text)
	}
}

// TestADriftRunStillClonesTheRepository covers a regression that shipped and
// that NOTHING in this suite could see: EnsureClone and Fetch lived inside
// runCommitLoop, which a drift run skips entirely, so a drift-only pass
// never cloned and every checkout it then attempted failed with
// "chdir /work/repo: no such file or directory".
//
// Preparing the workdir is unconditional and belongs above the DRIFT_ONLY
// branch, because every pass that gets past the gate goes on to check out a
// ref.
//
// ⚠️ IT WAS FOUND BY THE FIRST RUN AGAINST THE REAL CLUSTER, not by a
// test, and the reason is worth keeping: every fake git succeeds whether or
// not a clone happened, so "check out a ref in a directory that does not
// exist" has no counterpart in a fake. This test therefore asserts the CALL,
// which is the only thing a fake can observe.
func TestADriftRunStillClonesTheRepository(t *testing.T) {
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()

	git := &fakeGit{DirsAtRef: map[string][]string{"": {"platform"}, head: {"platform"}}}
	tofu := &fakeTofu{}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	deps.Cfg.DriftOnly = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, head)

	if !git.cloned() {
		t.Error("a drift run did not clone the repository, so every checkout it makes will fail")
	}
	if !git.fetched() {
		t.Error("a drift run did not fetch origin main, so it plans against a stale tree")
	}
	if git.tokenSeen() == "" {
		t.Error("a drift run never attached the installation token, so the clone is unauthenticated")
	}
}

// TestEveryTofuRunCarriesTheGitHubAppIdentity covers the third defect the
// trial runs against production found: buildBaseEnv returned only PATH and
// HOME, so the `github` provider's app_auth block had none of its required
// arguments and tofu refused at init with "Missing required argument ...
// pem_file / id / installation_id". Every root using that provider failed --
// the second trial run reported "drift UNKNOWN for: platform,
// projects/recipes".
//
// All four are needed: GH_TOKEN plus the three GITHUB_APP_*. Because the
// child environment is constructed rather than inherited, a variable a
// provider needs and this function does not name simply is not there.
//
// ⚠️ GITHUB_APP_PEM_FILE IS THE KEY'S CONTENTS, NOT A PATH, despite the name.
func TestEveryTofuRunCarriesTheGitHubAppIdentity(t *testing.T) {
	dir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)

	d := applyDeps{Dir: dir, PATH: "/usr/bin", HOME: "/root", Token: "gh-fixture"}
	env, err := buildBaseEnv(d, d.Token)
	if err != nil {
		t.Fatalf("buildBaseEnv: %v", err)
	}

	seen := map[string]string{}
	for _, kv := range env {
		if i := strings.Index(kv, "="); i > 0 {
			seen[kv[:i]] = kv[i+1:]
		}
	}
	for _, name := range []string{
		"PATH", "HOME", "GH_TOKEN",
		"GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID", "GITHUB_APP_PEM_FILE",
	} {
		if seen[name] == "" {
			t.Errorf("tofu's environment has no %s; the github provider's app_auth block needs it", name)
		}
	}
	// The PEM is the key itself, so it must look like one rather than a path.
	if !strings.Contains(seen["GITHUB_APP_PEM_FILE"], "PRIVATE KEY") {
		t.Errorf("GITHUB_APP_PEM_FILE = %q, want the key's contents, not a path", seen["GITHUB_APP_PEM_FILE"])
	}
}

// TestBuildBaseEnvRefusesAMirrorMissingTheApp: absent is not empty. A mirror
// without the github-app item must stop the pass, not hand tofu a blank
// identity that fails three steps later inside a provider.
func TestBuildBaseEnvRefusesAMirrorMissingTheApp(t *testing.T) {
	dir, _ := testSecretsDir(t) // nothing written
	d := applyDeps{Dir: dir, PATH: "/usr/bin", HOME: "/root"}
	if _, err := buildBaseEnv(d, ""); err == nil {
		t.Fatal("buildBaseEnv accepted a mirror with no github-app item")
	}
}

// TestTofuGetsTheOnePasswordServiceAccountToken covers the fourth gap in
// the constructed environment, found by the first non-drift trial against
// production. The credentials root declares a `onepassword` provider, which
// reads its credentials from the environment; without this tofu fails at plan
// with "Invalid provider configuration ... Service Account ... should be set",
// and the pass reports "tofu plan failed for credentials".
//
// ⚠️ THIS IS WHY $OP_TOKEN_FILE IS REQUIRED. It had been recorded as
// "required and read by nothing" -- wrong, and the worst of both: truss
// demanded the variable and never used it.
func TestTofuGetsTheOnePasswordServiceAccountToken(t *testing.T) {
	dir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)

	tokenPath := filepath.Join(t.TempDir(), "op")
	if err := os.WriteFile(tokenPath, []byte("op-fixture-value\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	cfg := testConfig()
	cfg.OPTokenFile = tokenPath
	d := applyDeps{Cfg: cfg, Dir: dir, PATH: "/usr/bin", HOME: "/root"}

	env, err := buildBaseEnv(d, "")
	if err != nil {
		t.Fatalf("buildBaseEnv: %v", err)
	}
	var got string
	for _, kv := range env {
		if strings.HasPrefix(kv, "OP_SERVICE_ACCOUNT_TOKEN=") {
			got = strings.TrimPrefix(kv, "OP_SERVICE_ACCOUNT_TOKEN=")
		}
	}
	if got == "" {
		t.Fatal("tofu's environment has no OP_SERVICE_ACCOUNT_TOKEN; the credentials root cannot plan")
	}
	// Trailing newline stripped: a token file written by an editor or by
	// `echo` ends in one, and it is not part of the credential.
	if got != "op-fixture-value" {
		t.Errorf("OP_SERVICE_ACCOUNT_TOKEN = %q, want the file's contents with the trailing newline stripped", got)
	}
}

// TestAnEmptyOnePasswordTokenIsRefused: absent is not empty. A zero-byte
// token file would authenticate as nobody and fail inside a provider three
// steps later.
func TestAnEmptyOnePasswordTokenIsRefused(t *testing.T) {
	dir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)

	tokenPath := filepath.Join(t.TempDir(), "op")
	if err := os.WriteFile(tokenPath, []byte("\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	cfg := testConfig()
	cfg.OPTokenFile = tokenPath
	d := applyDeps{Cfg: cfg, Dir: dir, PATH: "/usr/bin", HOME: "/root"}

	if _, err := buildBaseEnv(d, ""); err == nil {
		t.Fatal("buildBaseEnv accepted an empty 1Password token")
	}
}

// TestTheRepoAdminTokenIsOptionalAndReachesTofuAsAVariable covers the
// credential a consumer needs only if its roots CREATE repositories.
//
// ⚠️ ABSENT MUST NOT BE FATAL. A GitHub App cannot create a repository under
// a user account -- `POST /user/repos` is server-to-server: false -- so a
// consumer that creates them mounts a classic PAT, and one that only adopts
// existing repositories with import blocks never does. Making it required
// would stop every such deployment on an upgrade, for a credential it has no
// use for.
//
// ⚠️ AND IT MUST NOT BE GITHUB_TOKEN. The github provider reads that name
// from the environment, so exporting it would silently re-authenticate every
// github provider in the root -- including the default one that
// authenticates as the App, whose whole point is that it is not a person.
// TF_VAR_ is passed to one aliased provider explicitly.
func TestTheRepoAdminTokenIsOptionalAndReachesTofuAsAVariable(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		dir, write := testSecretsDir(t)
		writeGitHubAppSecret(t, write)

		d := applyDeps{Dir: dir, PATH: "/usr/bin", HOME: "/root", Token: "gh-fixture"}
		env, err := buildBaseEnv(d, d.Token)
		if err != nil {
			t.Fatalf("an unmounted repo-admin token was fatal: %v", err)
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "TF_VAR_github_repo_admin_token=") {
				t.Fatal("a token nobody mounted reached tofu")
			}
		}
	})

	t.Run("present", func(t *testing.T) {
		dir, write := testSecretsDir(t)
		writeGitHubAppSecret(t, write)
		write("github-repo-admin", "password", "ghp_fixture_value")

		d := applyDeps{Dir: dir, PATH: "/usr/bin", HOME: "/root", Token: "gh-fixture"}
		env, err := buildBaseEnv(d, d.Token)
		if err != nil {
			t.Fatalf("buildBaseEnv: %v", err)
		}
		var got string
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "TF_VAR_github_repo_admin_token="); ok {
				got = v
			}
			if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
				t.Error("the PAT was exported as GITHUB_TOKEN, which every github provider reads")
			}
		}
		if got != "ghp_fixture_value" {
			t.Fatalf("TF_VAR_github_repo_admin_token = %q, want the mounted value", got)
		}
	})
}
