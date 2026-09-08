package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/secrets"
)

// buildTestDeps assembles applyDeps against a fake ledger (real
// ledger.Journal/Store over httptest), a fake Vault (real secrets.KV over
// httptest, empty mount), a fake Telegram reached through a redirecting
// HTTP client, and whatever gitDriver/forgeGateway/tofuFactory the caller
// supplies. It returns the deps plus the fake ledger and fake Telegram so
// a test can inspect what was written and sent.
func buildTestDeps(t *testing.T, forgeFake *fakeForge, git gitDriver, newTofu tofuFactory) (applyDeps, *fakeLedger, *fakeTelegram) {
	t.Helper()

	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	write(itemCFTokenMint, fieldCFCredential, "fake-mint-token")
	write(itemGCPApply, fieldGCPCredentials, "fake-google-creds")
	write(itemTofuEncryption, fieldTofuPassphrase, "fake-passphrase")
	write(itemCFInfraAdmin, fieldCFPassword, "fake-infra-tok")

	vaultSrv := newFakeVault(t, "platform")

	store, err := ledger.New(ledger.Config{
		Endpoint:        fl.endpoint(),
		Bucket:          "state-bucket",
		AccessKeyID:     "AKIAFAKEACCESSKEYID",
		SecretAccessKey: "fakesecretaccesskeyfakesecretaccesskey",
	})
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	journal := &ledger.Journal{Store: store, Layout: ledger.Layout{
		AppliedPrefix: "applied", FailedPrefix: "failed",
		HeadKey: "head", HeartbeatKey: "heartbeat", PlanDigestPrefix: "digests",
	}}

	ft := newFakeTelegram(t)

	deps := applyDeps{
		Cfg:     testConfig(),
		Dir:     dir,
		Journal: journal,
		Forge:   forgeFake,
		Telegram: notify.Telegram{
			BotToken: "fake-bot-token",
			ChatID:   "-100200300",
			HTTP:     redirectingHTTPClient(ft.srv.URL),
		},
		Git:     git,
		NewTofu: newTofu,
		Now:     func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) },
		VaultConfig: secrets.KVConfig{
			Addr: vaultSrv.URL, Mount: "platform", Role: "applier", JWTPath: testJWTFile(t),
		},
		PATH: "/usr/bin", HOME: "/root",
	}
	return deps, fl, ft
}

// testConfig is a minimal config.Config, built by hand rather than through
// config.Load -- these tests exercise runApplyPass directly, below the
// layer that reads the environment.
func testConfig() config.Config {
	return config.Config{
		Repo:                "acme/platform",
		Approver:            "alice",
		LedgerBucket:        "state-bucket",
		LedgerAppliedPrefix: "applied",
		LedgerFailedPrefix:  "failed",
		LedgerHeadKey:       "head",
		HeartbeatKey:        "heartbeat",
		PlanDigestPrefix:    "digests",
		Workdir:             "",
		RequiredCheck:       "plan",
		ExpiryWarnDays:      30,
	}
}

// TestApplyAlwaysWritesAHeartbeatAndAlerts drives a pass that fails at the
// branch-protection gate -- the simplest failure this binary can produce,
// needing no git or tofu activity at all -- and checks that a heartbeat
// still lands in the ledger and a message still reaches Telegram (§2 item
// 8): "the pass always writes a heartbeat and always sends a message...
// and exits 1 if and only if failure is non-empty."
func TestApplyAlwaysWritesAHeartbeatAndAlerts(t *testing.T) {
	forgeFake := &fakeForge{
		// Zero-value gates.Protection: every pointer nil, which
		// CheckProtection refuses (absent is not compliant).
	}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, ft := buildTestDeps(t, forgeFake, git, newTofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "headsha1")

	if result.failure == "" {
		t.Fatalf("result.failure is empty, want the branch-protection refusal")
	}

	hbBytes, ok := fl.get("heartbeat")
	if !ok {
		t.Fatalf("no heartbeat object was written to the ledger")
	}
	var hb ledger.Heartbeat
	if err := json.Unmarshal(hbBytes, &hb); err != nil {
		t.Fatalf("heartbeat did not parse as JSON: %v\n%s", err, hbBytes)
	}
	if hb.Failure == nil || *hb.Failure == "" {
		t.Fatalf("heartbeat.Failure = %v, want the refusal reason", hb.Failure)
	}
	if hb.Time == "" {
		t.Fatalf("heartbeat.Time is empty")
	}

	text := ft.lastText()
	if text == "" {
		t.Fatalf("no message reached the fake Telegram server")
	}
	if !strings.Contains(text, "FAILED") {
		t.Fatalf("telegram text = %q, want it to lead with FAILED", text)
	}
}
