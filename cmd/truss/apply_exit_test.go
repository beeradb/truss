package main

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// applyExitCode mirrors cmdApply's own translation from an applyResult to
// a process exit code (§4.9: "apply: 0 unless failure is set"), so a test
// that only has an applyResult (because it drove runApplyPass directly,
// below the environment-reading layer) can still assert the contract by
// the same rule cmdApply uses.
func applyExitCode(r applyResult) int {
	if r.failure != "" {
		return 1
	}
	return 0
}

// TestApplyExitsNonZeroIfAndOnlyIfThereIsAFailure checks both directions of
// the biconditional in §4.9: a clean pass (no failure at any stage) exits
// 0, and a pass that fails ANYWHERE -- here, at the branch-protection gate,
// exercised through the real cmdApply/config.Load/ledger/forge stack so the
// check is not just of runApplyPass in isolation -- exits 1.
func TestApplyExitsNonZeroIfAndOnlyIfThereIsAFailure(t *testing.T) {
	t.Run("failure exits 1", func(t *testing.T) {
		dir, write := testSecretsDir(t)
		fl := newFakeLedger(t, "state-bucket")
		writeLedgerSecret(t, write, fl)
		writeGitHubAppSecret(t, write)
		write(itemTelegram, fieldTelegramBotToken, "fake-bot-token")
		write(itemTelegram, fieldTelegramChatID, "-100200300")
		write(itemCFTokenMint, fieldCFCredential, "fake-mint-token")
		fl.objects["head"] = []byte("headsha1")

		vaultSrv := productionShapedVault(t, "platform")
		forgeSrv := newFakeForge(t, "irrelevant-sha", fakeForgeScenario{
			Approver:   "alice",
			Protection: map[string]any{}, // every key absent -- CheckProtection refuses all of them
		})

		env := testFullEnv(dir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
		env = withForgeBaseURL(env, forgeSrv.URL)
		env = withVaultAddr(env, vaultSrv.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		code := runEnv(ctx, []string{"apply"}, env, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
		}
	})

	t.Run("clean pass exits 0", func(t *testing.T) {
		forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
		git := &fakeGit{
			CommitsList: nil, // origin/main == last: nothing to do
			HasDirFn:    func(string) bool { return false },
		}
		newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

		deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "headsha1")

		if code := applyExitCode(result); code != 0 {
			t.Fatalf("exit code = %d, want 0 (failure: %q)", code, result.failure)
		}
	})
}
