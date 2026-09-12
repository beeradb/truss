package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// statusHeartbeat builds a heartbeat body written at t, with an optional
// failure -- the same shape apply_cmd.go's cmdApply writes, field for
// field, so a test here is driving `truss status` against exactly what
// production produces.
func statusHeartbeat(t time.Time, lastSHA string, applied, noop int, failure *string) []byte {
	body, _ := json.Marshal(map[string]any{
		"time":     t.UTC().Format(heartbeatTimeLayout),
		"last_sha": lastSHA,
		"applied":  applied,
		"noop":     noop,
		"failure":  failure,
		"rotation": nil,
		"drift":    nil,
		"expiring": []map[string]any{
			{"name": "cf-token-mint", "days_left": 5},
		},
	})
	return body
}

// TestStalenessIsDerivedFromThePassIntervalNotAConstant pins the formula
// itself: three missed passes, at whatever interval is given, not a
// number restated independently of the schedule it describes.
func TestStalenessIsDerivedFromThePassIntervalNotAConstant(t *testing.T) {
	if got, want := staleAfter(time.Minute), 3*time.Minute; got != want {
		t.Errorf("staleAfter(1m) = %v, want %v", got, want)
	}
	if got, want := staleAfter(5*time.Minute), 15*time.Minute; got != want {
		t.Errorf("staleAfter(5m) = %v, want %v (the old CronJob-era constant, derived rather than restated)", got, want)
	}
	if defaultStaleAfter != staleAfter(defaultLoopInterval) {
		t.Errorf("defaultStaleAfter = %v, want staleAfter(defaultLoopInterval) = %v -- they must be the same formula, not two numbers that happen to agree", defaultStaleAfter, staleAfter(defaultLoopInterval))
	}
}

// TestStatusHealthyExitsZeroAndNamesShaAndAge is the clean case: a fresh
// heartbeat, no failure. It also carries the "names the last sha and the
// age" assertion the spec calls for, since a healthy run is the simplest
// place to check both are actually printed.
func TestStatusHealthyExitsZeroAndNamesShaAndAge(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("head", []byte("abc123headsha"))
	fl.put("heartbeat", statusHeartbeat(time.Now(), "abc123headsha", 3, 1, nil))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "abc123headsha") {
		t.Errorf("stdout = %q, want it to name the last sha", out)
	}
	if !strings.Contains(out, "ago") {
		t.Errorf("stdout = %q, want it to say how long ago the heartbeat was written", out)
	}
}

// TestStatusRecordedFailureExitsOne: a clean, fresh heartbeat that
// nonetheless recorded a failure on the last pass must not read as
// healthy.
func TestStatusRecordedFailureExitsOne(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	failure := "tofu apply failed for platform"
	fl.put("head", []byte("headsha"))
	fl.put("heartbeat", statusHeartbeat(time.Now(), "headsha", 0, 0, &failure))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), failure) {
		t.Errorf("stdout = %q, want it to name the recorded failure", stdout.String())
	}
}

// TestStatusStaleHeartbeatExitsOne is the check the whole command exists
// for: a heartbeat with no failure at all, but written long enough ago
// that the applier may have stopped running, must not read as healthy --
// breaking this specific comparison (see the report for how it was
// watched failing) is exactly what a status command that reads only
// outcome fields would get wrong.
func TestStatusStaleHeartbeatExitsOne(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	old := time.Now().Add(-1 * time.Hour)
	fl.put("head", []byte("headsha"))
	fl.put("heartbeat", statusHeartbeat(old, "headsha", 5, 2, nil))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(strings.ToLower(stdout.String()), "stale") {
		t.Errorf("stdout = %q, want it to say the heartbeat is stale", stdout.String())
	}
}

// TestStatusAbsentHeartbeatExitsTwo: HEAD is there but the applier has
// never completed a pass, so there is nothing to report on -- "could not
// ask", not a refusal.
func TestStatusAbsentHeartbeatExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("head", []byte("headsha"))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}

// TestStatusAbsentHeadExitsTwo: a heartbeat with no HEAD at all means
// bootstrap never ran (§2 item 5) -- also "could not ask".
func TestStatusAbsentHeadExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("heartbeat", statusHeartbeat(time.Now(), "headsha", 1, 0, nil))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}

// TestStatusUnparseableHeartbeatExitsTwo: garbage at the heartbeat key is
// "could not ask", never a guessed default.
func TestStatusUnparseableHeartbeatExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("head", []byte("headsha"))
	fl.put("heartbeat", []byte("not json"))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}

// TestStatusBadStaleAfterExitsTwo: an unparseable --stale-after must
// refuse outright rather than silently falling back to the default --
// house rule 2, "no fallbacks, one path".
func TestStatusBadStaleAfterExitsTwo(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.put("head", []byte("headsha"))
	fl.put("heartbeat", statusHeartbeat(time.Now(), "headsha", 1, 0, nil))

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"status", "--stale-after", "bogus"}, env, nil, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
	}
}
