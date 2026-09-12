package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadLoopConfigRefusesDriftCheck(t *testing.T) {
	getenv := func(name string) string {
		if name == "DRIFT_CHECK" {
			return "1"
		}
		return ""
	}
	_, problems := loadLoopConfig(getenv)
	if len(problems) == 0 {
		t.Fatalf("loadLoopConfig accepted $DRIFT_CHECK, want a refusal")
	}
	if !strings.Contains(problems[0], "DRIFT_CHECK") {
		t.Errorf("problem = %q, want it to name DRIFT_CHECK", problems[0])
	}
}

func TestLoadLoopConfigRefusesDriftCheckEvenSetToZero(t *testing.T) {
	// $DRIFT_CHECK=0 is a valid, meaningful value to config.Load (it means
	// "frequent pass"), but the loop refuses it anyway: the rule is that the
	// variable must be ABSENT, not that it must carry any particular value,
	// because its mere presence is a sign the manifest still thinks in
	// CronJob terms.
	getenv := func(name string) string {
		if name == "DRIFT_CHECK" {
			return "0"
		}
		return ""
	}
	_, problems := loadLoopConfig(getenv)
	if len(problems) == 0 {
		t.Fatalf("loadLoopConfig accepted $DRIFT_CHECK=0, want a refusal")
	}
}

func TestLoadLoopConfigDefaultsTheIntervalToOneMinute(t *testing.T) {
	lcfg, problems := loadLoopConfig(func(string) string { return "" })
	if len(problems) != 0 {
		t.Fatalf("loadLoopConfig: %v", problems)
	}
	if lcfg.interval != time.Minute {
		t.Errorf("interval = %v, want %v", lcfg.interval, time.Minute)
	}
}

func TestLoadLoopConfigParsesAnExplicitInterval(t *testing.T) {
	getenv := func(name string) string {
		if name == "LOOP_INTERVAL" {
			return "30s"
		}
		return ""
	}
	lcfg, problems := loadLoopConfig(getenv)
	if len(problems) != 0 {
		t.Fatalf("loadLoopConfig: %v", problems)
	}
	if lcfg.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", lcfg.interval)
	}
}

func TestLoadLoopConfigRefusesAnUnparseableInterval(t *testing.T) {
	getenv := func(name string) string {
		if name == "LOOP_INTERVAL" {
			return "banana"
		}
		return ""
	}
	_, problems := loadLoopConfig(getenv)
	if len(problems) == 0 {
		t.Fatalf("loadLoopConfig accepted an unparseable $LOOP_INTERVAL, want a refusal")
	}
}

func TestLoadLoopConfigRefusesAZeroOrNegativeInterval(t *testing.T) {
	for _, raw := range []string{"0s", "-1m"} {
		getenv := func(name string) string {
			if name == "LOOP_INTERVAL" {
				return raw
			}
			return ""
		}
		if _, problems := loadLoopConfig(getenv); len(problems) == 0 {
			t.Errorf("loadLoopConfig accepted $LOOP_INTERVAL=%q, want a refusal", raw)
		}
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
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, io.Discard, io.Discard) }()

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
	go func() { done <- runLoop(ctx, 20*time.Millisecond, ft.turn, io.Discard, io.Discard) }()

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
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, io.Discard, io.Discard) }()

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

	code := runLoop(context.Background(), time.Hour, ft.turn, io.Discard, &stderr)

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
	go func() { done <- runLoop(ctx, time.Hour, ft.turn, &stdout, io.Discard) }()

	waitForCount(t, ft, 1, 2*time.Second)
	cancel()
	<-done

	if !strings.Contains(stdout.String(), "OK: nothing to do") {
		t.Errorf("stdout = %q, want the pass's notify text", stdout.String())
	}
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
