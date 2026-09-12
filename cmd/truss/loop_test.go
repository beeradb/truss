package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLoadLoopConfigRefusesDriftCheck(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_CHECK": "1"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_CHECK") {
		t.Fatalf("problems = %v, want exactly one refusal naming DRIFT_CHECK", problems)
	}
}

func TestLoadLoopConfigRefusesDriftCheckEvenSetToZero(t *testing.T) {
	// $DRIFT_CHECK=0 is a valid, meaningful value to config.Load (it means
	// "frequent pass"), but the loop refuses it anyway: the rule is that the
	// variable must be ABSENT, not that it must carry any particular value,
	// because its mere presence is a sign the manifest still thinks in
	// CronJob terms.
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_CHECK": "0"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_CHECK") {
		t.Fatalf("problems = %v, want exactly one refusal naming DRIFT_CHECK", problems)
	}
}

// validLoopEnv answers every variable loadLoopConfig requires with a value
// that passes, so a test about ONE variable does not have to restate every
// other one. overrides layer on top, the same shape testFullEnv uses.
func validLoopEnv(overrides map[string]string) func(string) string {
	base := map[string]string{
		"DRIFT_AT":            "04:10",
		"DRIFT_HEARTBEAT_KEY": "heartbeat/drift.json",
		"HEARTBEAT_KEY":       "heartbeat/applier.json",
		"HANDOFF_SOCKET":      "/var/run/publish/publish.sock",
		// "localhost", not a literal IP: scripts/leakscan refuses any IPv4
		// dotted quad anywhere in the tree, with no exemption but the
		// control listener's own compiled-in bind address.
		"METRICS_LISTEN": "localhost:0",
		"CONTROL_PORT":   "0",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(name string) string { return base[name] }
}

func TestLoadLoopConfigDefaultsTheIntervalToOneMinute(t *testing.T) {
	lcfg, problems := loadLoopConfig(validLoopEnv(nil))
	if len(problems) != 0 {
		t.Fatalf("loadLoopConfig: %v", problems)
	}
	if lcfg.interval != time.Minute {
		t.Errorf("interval = %v, want %v", lcfg.interval, time.Minute)
	}
}

func TestLoadLoopConfigParsesAnExplicitInterval(t *testing.T) {
	lcfg, problems := loadLoopConfig(validLoopEnv(map[string]string{"LOOP_INTERVAL": "30s"}))
	if len(problems) != 0 {
		t.Fatalf("loadLoopConfig: %v", problems)
	}
	if lcfg.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", lcfg.interval)
	}
}

func TestLoadLoopConfigRefusesAnUnparseableInterval(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"LOOP_INTERVAL": "banana"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "LOOP_INTERVAL") {
		t.Fatalf("problems = %v, want exactly one refusal naming LOOP_INTERVAL", problems)
	}
}

func TestLoadLoopConfigRefusesAZeroOrNegativeInterval(t *testing.T) {
	for _, raw := range []string{"0s", "-1m"} {
		_, problems := loadLoopConfig(validLoopEnv(map[string]string{"LOOP_INTERVAL": raw}))
		if len(problems) != 1 || !strings.Contains(problems[0], "LOOP_INTERVAL") {
			t.Errorf("$LOOP_INTERVAL=%q: problems = %v, want exactly one refusal naming LOOP_INTERVAL", raw, problems)
		}
	}
}

func TestLoadLoopConfigRefusesAMissingDriftAt(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_AT": ""}))
	if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_AT") {
		t.Fatalf("problems = %v, want exactly one refusal naming DRIFT_AT", problems)
	}
}

func TestLoadLoopConfigRefusesADriftAtNotShapedHHMM(t *testing.T) {
	for _, raw := range []string{"04:10:00", "tomorrow", "25:00", "04:1"} {
		_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_AT": raw}))
		if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_AT") {
			t.Errorf("$DRIFT_AT=%q: problems = %v, want exactly one refusal naming DRIFT_AT", raw, problems)
		}
	}
}

func TestLoadLoopConfigParsesADriftAtInUTC(t *testing.T) {
	lcfg, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_AT": "04:10"}))
	if len(problems) != 0 {
		t.Fatalf("loadLoopConfig: %v", problems)
	}
	if lcfg.driftAt.hour != 4 || lcfg.driftAt.minute != 10 {
		t.Errorf("driftAt = %+v, want 04:10", lcfg.driftAt)
	}
}

func TestLoadLoopConfigRefusesAMissingDriftHeartbeatKey(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_HEARTBEAT_KEY": ""}))
	if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_HEARTBEAT_KEY") {
		t.Fatalf("problems = %v, want exactly one refusal naming DRIFT_HEARTBEAT_KEY", problems)
	}
}

func TestLoadLoopConfigRefusesADriftHeartbeatKeyEqualToHeartbeatKey(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"DRIFT_HEARTBEAT_KEY": "heartbeat/applier.json"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "DRIFT_HEARTBEAT_KEY") {
		t.Fatalf("problems = %v, want exactly one refusal naming DRIFT_HEARTBEAT_KEY", problems)
	}
}

func TestLoadLoopConfigRefusesAMissingHandoffSocket(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"HANDOFF_SOCKET": ""}))
	if len(problems) != 1 || !strings.Contains(problems[0], "HANDOFF_SOCKET") {
		t.Fatalf("problems = %v, want exactly one refusal naming HANDOFF_SOCKET", problems)
	}
}

func TestLoadLoopConfigRefusesAMissingControlPort(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"CONTROL_PORT": ""}))
	if len(problems) != 1 || !strings.Contains(problems[0], "CONTROL_PORT") {
		t.Fatalf("problems = %v, want exactly one refusal naming CONTROL_PORT", problems)
	}
}

func TestLoadLoopConfigRefusesANonNumericControlPort(t *testing.T) {
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"CONTROL_PORT": "http"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "CONTROL_PORT") {
		t.Fatalf("problems = %v, want exactly one refusal naming CONTROL_PORT", problems)
	}
}

func TestLoadLoopConfigRefusesAHostPortStringAsControlPort(t *testing.T) {
	// A host:port string is a bind address wearing a disguise -- the bind
	// host is compiled in and must stay that way.
	_, problems := loadLoopConfig(validLoopEnv(map[string]string{"CONTROL_PORT": "somehost:9999"}))
	if len(problems) != 1 || !strings.Contains(problems[0], "CONTROL_PORT") {
		t.Fatalf("problems = %v, want exactly one refusal naming CONTROL_PORT", problems)
	}
}

func TestDriftScheduleDueOncePerDayAtTheWindow(t *testing.T) {
	s := driftSchedule{hour: 4, minute: 10}
	window := time.Date(2026, 9, 12, 4, 10, 0, 0, time.UTC)

	if s.due(window.Add(-time.Second), time.Time{}) {
		t.Errorf("due before the window opened, want not due")
	}
	if !s.due(window, time.Time{}) {
		t.Errorf("not due exactly at the window, want due")
	}
	if !s.due(window.Add(time.Hour), time.Time{}) {
		t.Errorf("not due an hour after the window, want due (never drifted)")
	}
	if s.due(window.Add(time.Hour), window) {
		t.Errorf("due again after already drifting today, want not due")
	}
}

func TestDriftScheduleCatchesUpAfterADowntimeButOnlyOnce(t *testing.T) {
	s := driftSchedule{hour: 4, minute: 10}
	lastDrift := time.Date(2026, 9, 10, 4, 10, 0, 0, time.UTC) // two days stale

	comeBackUp := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	if !s.due(comeBackUp, lastDrift) {
		t.Fatalf("a loop that missed two windows should drift once on return")
	}
	// After running, lastDrift advances to "now" (realLoopTurn's own
	// behavior) -- confirm that reads as satisfied for the rest of today.
	if s.due(comeBackUp.Add(time.Hour), comeBackUp) {
		t.Errorf("due again later the same day after catching up, want not due")
	}
}

// fakeTurn is loopTurn's test double: it counts how many times it was
// called and lets a test script what each call reports.
type fakeTurn struct {
	mu    sync.Mutex
	calls int
	// fatalAt, if non-zero, makes the call at that count (1-indexed) fatal.
	fatalAt int
	text    string
}

func (f *fakeTurn) turn(ctx context.Context) (string, bool, []string) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.fatalAt != 0 && n == f.fatalAt {
		return "", true, []string{"refusing to start: simulated construction failure"}
	}
	return f.text, false, nil
}

func (f *fakeTurn) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRunLoopCallsTurnImmediatelyWithoutWaitingForATick is the property
// that makes `truss loop` behave like `truss apply` on its very first
// pass: an operator restarting the daemon should not wait a full interval
// before anything happens.
func TestRunLoopCallsTurnImmediatelyWithoutWaitingForATick(t *testing.T) {
	ft := &fakeTurn{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, neverStop, nil, io.Discard, io.Discard) }()

	waitForCount(t, ft, 1, 2*time.Second)
	cancel()
	<-done
}

// TestRunLoopTicksAgainAfterTheInterval proves the SECOND call happens on
// the configured cadence, not merely that a first call happens at all.
func TestRunLoopTicksAgainAfterTheInterval(t *testing.T) {
	ft := &fakeTurn{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- runLoop(ctx, 20*time.Millisecond, ft.turn, neverStop, nil, io.Discard, io.Discard) }()

	waitForCount(t, ft, 3, 2*time.Second)
	cancel()
	<-done
}

// TestRunLoopStopsBetweenTicksWhenTheContextIsDone is the mechanism a
// future signal handler builds on: ctx.Done() is checked at the top of
// each turn, never mid-turn, so this proves the loop stops rather than
// running forever once asked to.
func TestRunLoopStopsBetweenTicksWhenTheContextIsDone(t *testing.T) {
	ft := &fakeTurn{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, neverStop, nil, io.Discard, io.Discard) }()

	waitForCount(t, ft, 1, 2*time.Second)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 for a context-cancelled stop", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("runLoop did not return within 2s of cancellation")
	}
}

// TestRunLoopStopsOnAFatalTurnAndPrintsWhyWithoutASecondCall is the
// boot-time-refusal shape carried into the loop: a construction failure
// stops it exactly once, the same as `truss apply` exiting 1, rather than
// spinning on an error that ticking again cannot fix.
func TestRunLoopStopsOnAFatalTurnAndPrintsWhyWithoutASecondCall(t *testing.T) {
	ft := &fakeTurn{fatalAt: 1}
	var stderr bytes.Buffer

	code := runLoop(context.Background(), time.Hour, ft.turn, neverStop, nil, io.Discard, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if ft.count() != 1 {
		t.Errorf("turn was called %d times, want exactly 1: a fatal turn must not be retried", ft.count())
	}
	if !strings.Contains(stderr.String(), "refusing to start") {
		t.Errorf("stderr = %q, want the refusal text", stderr.String())
	}
}

// TestRunLoopPrintsANonEmptyNotifyTextToStdout matches cmdApply's own
// contract: the pass's notify text, when non-empty, is the one thing this
// binary puts on stdout.
func TestRunLoopPrintsANonEmptyNotifyTextToStdout(t *testing.T) {
	ft := &fakeTurn{text: "OK: nothing to do"}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout bytes.Buffer

	done := make(chan int, 1)
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, neverStop, nil, &stdout, io.Discard) }()

	waitForCount(t, ft, 1, 2*time.Second)
	cancel()
	<-done

	if !strings.Contains(stdout.String(), "OK: nothing to do") {
		t.Errorf("stdout = %q, want the pass's notify text", stdout.String())
	}
}

// neverStop is the stopping func for every test that is not itself about
// stopping.
func neverStop() bool { return false }

// TestRunLoopStopsAfterTheCurrentTurnWhenStoppingIsTrue is runLoop's own
// half of graceful stop: once stopping() reports true, right after a turn
// returns, the loop must exit without waiting for ctx.Done() and without
// starting a second turn.
func TestRunLoopStopsAfterTheCurrentTurnWhenStoppingIsTrue(t *testing.T) {
	ft := &fakeTurn{}
	var stop atomic.Bool
	// Flips true from inside the first call, mirroring how a signal
	// handler flips the same flag while a turn (a whole pass) is in
	// flight.
	turn := func(ctx context.Context) (string, bool, []string) {
		s, f, p := ft.turn(ctx)
		stop.Store(true)
		return s, f, p
	}

	code := runLoop(context.Background(), time.Hour, turn, stop.Load, nil, io.Discard, io.Discard)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ft.count() != 1 {
		t.Errorf("turn was called %d times, want exactly 1: stopping() true after the first call must prevent a second", ft.count())
	}
}

// TestRunLoopRunsATurnWhenRunNowFires proves the control API's seam into
// the loop: a send on runNow (from a different goroutine, as
// control_server.go's handler does) triggers a turn without waiting for
// the tick.
func TestRunLoopRunsATurnWhenRunNowFires(t *testing.T) {
	ft := &fakeTurn{}
	runNow := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, neverStop, runNow, io.Discard, io.Discard) }()

	waitForCount(t, ft, 1, 2*time.Second) // the immediate call
	runNow <- struct{}{}
	waitForCount(t, ft, 2, 2*time.Second) // triggered by runNow, not the (1-hour) tick

	cancel()
	<-done
}

func waitForCount(t *testing.T, ft *fakeTurn, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ft.count() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("turn was called %d times within %s, want at least %d", ft.count(), timeout, want)
}

// TestASigtermAsksTheLoopToStopWithoutCancellingTheContext installs the
// real signal handler and sends this test process a real SIGTERM, the one
// piece of the graceful-stop chain the fake-based tests above cannot
// reach: proof that signal.Notify is wired to SIGTERM (and SIGINT) at all,
// and that the context handed to a turn is NOT the thing cancelled --
// docs/work-items.md records a harness that twice killed its own test
// suite doing signal work, so this test arms the handler, sends exactly
// one signal, and bounds its own wait rather than risking a hang.
func TestASigtermAsksTheLoopToStopWithoutCancellingTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopping, cleanup := armStopSignal(ctx, io.Discard)
	defer cleanup()

	if stopping() {
		t.Fatalf("stopping() is true before any signal was sent")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM to self: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !stopping() {
		if time.Now().After(deadline) {
			t.Fatalf("stopping() did not become true within 2s of SIGTERM")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if ctx.Err() != nil {
		t.Errorf("ctx.Err() = %v, want nil: SIGTERM must set a flag, never cancel the context a pass is running with", ctx.Err())
	}
}
