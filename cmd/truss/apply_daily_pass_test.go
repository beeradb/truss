package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/ledger"
)

// readHeartbeat fetches the pass's own heartbeat object back out of the
// fake ledger and parses it, the same way a real reader (or the next pass,
// or a human debugging one) would.
func readHeartbeat(t *testing.T, fl *fakeLedger) ledger.Heartbeat {
	t.Helper()
	b, ok := fl.get("heartbeat")
	if !ok {
		t.Fatal("no heartbeat object was written to the ledger")
	}
	var hb ledger.Heartbeat
	if err := json.Unmarshal(b, &hb); err != nil {
		t.Fatalf("heartbeat did not parse as JSON: %v\n%s", err, b)
	}
	return hb
}

// dailyPassDeps builds a pass that would apply cleanly on either schedule:
// compliant branch protection and commit gate, and a git fixture where
// every root "exists" (HasDirFn always true) so rotation and drift both
// have something to act on if given the chance. The point of each test
// below is which of them gets that chance.
func dailyPassDeps(t *testing.T, driftOnly bool, tofu *fakeTofu) (applyDeps, *fakeLedger, *fakeTelegram) {
	t.Helper()
	const head = "headsha1"
	forgeFake := compliantCommitGate("alice", head, head)
	forgeFake.ProtectionResult = compliantGatesProtection()
	git := &fakeGit{HasDirFn: func(string) bool { return true }}
	deps, fl, ft := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
	deps.Cfg.DriftOnly = driftOnly
	return deps, fl, ft
}

// TestRotationRunsOnTheDailyPassOnly pins FIX 1: apply.sh:1099-1105 rotates
// credentials only when DRIFT_ONLY=1, and truss had this exactly inverted
// -- rotation ran on the FREQUENT pass (three 1Password reads and a full
// `tofu plan` of credentials/ every fifteen minutes, apply.sh:1092 -- "most
// of the daily budget, spent to re-derive a date") and never on the daily
// pass, which silently stalls a 45-day rotation window forever rather than
// merely wasting a budget.
func TestRotationRunsOnTheDailyPassOnly(t *testing.T) {
	const head = "headsha1"

	t.Run("daily pass rotates", func(t *testing.T) {
		tofu := &fakeTofu{}
		deps, _, _ := dailyPassDeps(t, true, tofu)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runApplyPass(ctx, deps, head)

		if len(tofu.appliedDirs()) == 0 {
			t.Fatal("a drift (daily) pass never applied the credentials root -- rotation did not run")
		}
	})

	t.Run("frequent pass does not, with the exact bash skip string", func(t *testing.T) {
		tofu := &fakeTofu{}
		deps, fl, _ := dailyPassDeps(t, false, tofu)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runApplyPass(ctx, deps, head)

		if got := tofu.appliedDirs(); len(got) != 0 {
			t.Fatalf("a non-drift pass applied %v -- rotation must not run on the frequent pass", got)
		}

		hb := readHeartbeat(t, fl)
		// ⚠️ THE SKIP STRING MUST MATCH apply.sh:1103 BYTE FOR BYTE -- it
		// lands in the heartbeat and internal/parity compares it.
		if got := string(hb.Rotation); got != `{"skipped":"rotation runs on the daily pass"}` {
			t.Errorf("rotation summary = %s, want the exact apply.sh skip string", got)
		}
	})
}

// TestDriftAndRotationBothRunOnTheDailyPass is the one a naive fix breaks:
// it is not enough to move rotation into the driftRun branch, it has to run
// ALONGSIDE drift there, not instead of it. apply.sh:1099-1105 runs
// rotate_credentials THEN check_drift when DRIFT_ONLY=1 -- both, every
// time, in that order.
func TestDriftAndRotationBothRunOnTheDailyPass(t *testing.T) {
	const head = "headsha1"

	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, fl, _ := dailyPassDeps(t, true, tofu)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, head)

	if len(tofu.appliedDirs()) == 0 {
		t.Fatal("rotation did not apply the credentials root on the daily pass")
	}

	hb := readHeartbeat(t, fl)
	if !strings.Contains(string(hb.Drift), "platform") {
		t.Errorf("drift summary = %s, want platform reported drifted -- drift did not also run on the daily pass", hb.Drift)
	}
}

// TestTheExpirySweepRunsOnTheDailyPassOnly pins FIX 2: apply.sh:1110 is
// `[ "$DRIFT_ONLY" != "1" ] || check_credential_lifetimes` -- the sweep
// lists both vaults and reads every item's `expires` field, a question
// whose answer cannot change inside a day, and apply.sh:1081 records why
// asking it 288 times a day mattered: "asking it 288 times a day is most of
// what rate-limited the service account on 2026-09-07". Truss called it on
// every pass, unconditionally.
//
// The vault fixture (productionShapedVault, via buildTestDeps) is always
// "unusable" -- items, none with an expires -- so calling the sweep always
// produces an EXPIRY NOT CHECKED clause and not calling it always produces
// none. That makes the clause's presence an honest proxy for "was the sweep
// actually invoked", not just "did the pass mention expiry".
func TestTheExpirySweepRunsOnTheDailyPassOnly(t *testing.T) {
	const head = "headsha1"

	t.Run("non-drift pass never checks", func(t *testing.T) {
		tofu := &fakeTofu{}
		deps, _, ft := dailyPassDeps(t, false, tofu)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runApplyPass(ctx, deps, head)

		if text := ft.lastText(); strings.Contains(text, "EXPIRY NOT CHECKED") {
			t.Errorf("a non-drift pass reported on the expiry sweep, which must not run on it: %q", text)
		}
	})

	t.Run("drift pass checks", func(t *testing.T) {
		tofu := &fakeTofu{}
		deps, _, ft := dailyPassDeps(t, true, tofu)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runApplyPass(ctx, deps, head)

		if text := ft.lastText(); !strings.Contains(text, "EXPIRY NOT CHECKED") {
			t.Errorf("a drift pass did not report on the expiry sweep: %q", text)
		}
	})
}
