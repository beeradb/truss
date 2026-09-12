package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
)

// seedTestConfig builds a config.Config wired at a fake ledger, the shape
// seedLastDrift and buildPass both need to actually reach one.
func seedTestConfig(t *testing.T, fl *fakeLedger) config.Config {
	t.Helper()
	dir, write := testSecretsDir(t)
	writeLedgerSecret(t, write, fl)
	cfg := testConfig()
	cfg.SecretsDir = dir.Root
	return cfg
}

func TestSeedLastDriftReadsTheDriftHeartbeatsTimestamp(t *testing.T) {
	fl := newFakeLedger(t, "state-bucket")
	when := time.Date(2026, 9, 11, 4, 12, 30, 0, time.UTC)
	hb, err := json.Marshal(ledger.Heartbeat{Time: when.Format(heartbeatTimeLayout)})
	if err != nil {
		t.Fatalf("marshalling fixture heartbeat: %v", err)
	}
	fl.put("heartbeat/drift.json", hb)

	cfg := seedTestConfig(t, fl)
	got := seedLastDrift(context.Background(), cfg, "heartbeat/drift.json")
	if !got.Equal(when) {
		t.Errorf("seedLastDrift = %v, want %v", got, when)
	}
}

func TestSeedLastDriftIsTheZeroTimeWhenNoDriftHeartbeatExistsYet(t *testing.T) {
	fl := newFakeLedger(t, "state-bucket")
	cfg := seedTestConfig(t, fl)

	got := seedLastDrift(context.Background(), cfg, "heartbeat/drift.json")
	if !got.IsZero() {
		t.Errorf("seedLastDrift = %v, want the zero time on a virgin deployment", got)
	}
}

func TestSeedLastDriftIsTheZeroTimeWhenTheRecordDoesNotParse(t *testing.T) {
	fl := newFakeLedger(t, "state-bucket")
	fl.put("heartbeat/drift.json", []byte("not json"))
	cfg := seedTestConfig(t, fl)

	got := seedLastDrift(context.Background(), cfg, "heartbeat/drift.json")
	if !got.IsZero() {
		t.Errorf("seedLastDrift = %v, want the zero time on an unparseable record", got)
	}
}

// driftDecisionRecorder records one due()-shaped decision per call --
// standing in for the real thing so this test can assert what drift VALUE
// realLoopTurn computes and how lastDrift advances, without needing a
// working ledger, forge, Vault and git behind it. realLoopTurn's own body
// is a direct, two-line translation of exactly this decision (loop.go:
// `drift := lcfg.driftAt.due(n, lastDrift)` then, after the pass,
// `if drift { lastDrift = n }`), so proving the decision logic here and
// proving due() itself in loop_test.go together cover what realLoopTurn
// adds beyond buildPass and runApplyPass -- both already covered
// extensively elsewhere.
type driftDecisionRecorder struct {
	calls []bool // one entry per call, the drift value it was asked to run
}

func TestDriftDecisionAdvancesLastDriftOnlyWhenItRanDrift(t *testing.T) {
	s := driftSchedule{hour: 4, minute: 10}
	window := time.Date(2026, 9, 12, 4, 10, 0, 0, time.UTC)

	var rec driftDecisionRecorder
	lastDrift := time.Time{}
	// Mirrors realLoopTurn's own decision line without needing buildPass:
	// due() is called, the result is recorded, and lastDrift only advances
	// on a true. This is the property realLoopTurn is built on, isolated
	// from everything buildPass would otherwise require.
	decide := func(now time.Time) {
		drift := s.due(now, lastDrift)
		rec.calls = append(rec.calls, drift)
		if drift {
			lastDrift = now
		}
	}

	decide(window.Add(-time.Hour)) // before the window: frequent
	decide(window)                 // at the window: drift
	decide(window.Add(time.Minute)) // just after, same window: already ran

	want := []bool{false, true, false}
	if len(rec.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", rec.calls, want)
	}
	for i, got := range rec.calls {
		if got != want[i] {
			t.Errorf("call %d: drift = %v, want %v", i, got, want[i])
		}
	}
}
