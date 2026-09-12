package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
)

// defaultLoopInterval is the loop's pass interval when $LOOP_INTERVAL is
// unset -- a stated design decision (the loop-mode work), not a guess.
const defaultLoopInterval = time.Minute

// driftSchedule is the daily window $DRIFT_AT names, always in UTC -- never
// local time, because a local-time schedule moves twice a year under DST,
// and a credential rotation that happens twice or not at all on one day in
// October is not a schedule.
type driftSchedule struct {
	hour, minute int
}

// parseDriftAt parses "HH:MM" -- time.Parse's own "15:04" layout, which
// already refuses an out-of-range hour or minute.
func parseDriftAt(raw string) (driftSchedule, bool) {
	t, err := time.Parse("15:04", raw)
	if err != nil {
		return driftSchedule{}, false
	}
	return driftSchedule{hour: t.Hour(), minute: t.Minute()}, true
}

// due reports whether the daily drift window has opened and this loop has
// not acted on it yet. One comparison gets three cases right at once: a
// drift runs once a day; a loop that was down at the window runs one when
// it comes back (not one per missed day, and not none); and a loop that
// restarts after today's drift does not run a second one.
func (s driftSchedule) due(now, lastDrift time.Time) bool {
	window := time.Date(now.Year(), now.Month(), now.Day(), s.hour, s.minute, 0, 0, time.UTC)
	return !now.Before(window) && lastDrift.Before(window)
}

// loopConfig is `truss loop`'s own environment, layered on top of passEnv
// (apply_cmd.go), which it shares with `truss apply`.
type loopConfig struct {
	interval          time.Duration
	driftAt           driftSchedule
	driftHeartbeatKey string
	handoffSocket     string
	metricsListen     string
}

// loadLoopConfig reads and validates the loop's own environment on top of
// whatever loadPassEnv already validated. Fail-closed, matching
// config.Load's own contract: every problem reported, nothing defaulted
// that matters.
func loadLoopConfig(getenv func(string) string) (loopConfig, []string) {
	var problems []string

	// ⚠️ $DRIFT_CHECK IS A LEFTOVER FROM THE DRIFT CRONJOB AND MUST NEVER
	// REACH THE LOOP. config.Load turns "1" into cfg.DriftOnly=true, which
	// would make EVERY pass in the loop a drift pass -- 1,440 credential
	// rotations a day instead of one. The loop decides frequent vs. drift
	// itself, on its own schedule; the environment does not get a vote.
	if getenv("DRIFT_CHECK") != "" {
		problems = append(problems, "refusing to start: $DRIFT_CHECK must not be set on the loop -- it decides frequent vs. drift itself, on its own schedule")
	}

	interval := defaultLoopInterval
	if raw := getenv("LOOP_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			problems = append(problems, fmt.Sprintf("refusing to start: $LOOP_INTERVAL must be a positive duration, not %q", raw))
		} else {
			interval = d
		}
	}

	var driftAt driftSchedule
	if raw := getenv("DRIFT_AT"); raw == "" {
		problems = append(problems, "refusing to start: $DRIFT_AT is unset -- the loop needs to know when to run the daily drift pass, as HH:MM in UTC")
	} else if parsed, ok := parseDriftAt(raw); !ok {
		problems = append(problems, fmt.Sprintf("refusing to start: $DRIFT_AT must be HH:MM in UTC, not %q", raw))
	} else {
		driftAt = parsed
	}

	// ⚠️ MUST DIFFER FROM $HEARTBEAT_KEY. The two CronJobs gave the drift
	// pass its own heartbeat key for free; collapsing the two in one loop
	// would let a frequent pass overwrite the daily drift and expiry
	// findings a minute after they were produced.
	driftHeartbeatKey := getenv("DRIFT_HEARTBEAT_KEY")
	if driftHeartbeatKey == "" {
		problems = append(problems, "refusing to start: $DRIFT_HEARTBEAT_KEY is unset -- the drift pass needs its own heartbeat key, or a frequent pass a minute later would overwrite its record")
	} else if driftHeartbeatKey == getenv("HEARTBEAT_KEY") {
		problems = append(problems, "refusing to start: $DRIFT_HEARTBEAT_KEY must differ from $HEARTBEAT_KEY -- the same key would let a frequent pass overwrite the daily drift record a minute after it was written")
	}

	// ⚠️ REQUIRED UNCONDITIONALLY, UNLIKE `truss apply`'s
	// loadHandoffConfig. The loop always eventually runs a drift pass, so a
	// manifest without the publisher sidecar must be a refusal to start
	// regardless of which pass happens to be running at the instant it is
	// checked. Which pass actually DIALS the socket is decided by drift
	// alone (runApplyPass gates runHandoff on driftRun), so a frequent pass
	// with this set simply never reaches it -- the prohibition
	// loadHandoffConfig enforces for `truss apply` is satisfied here by
	// construction instead of by config.
	handoffSocket := getenv("HANDOFF_SOCKET")
	if handoffSocket == "" {
		problems = append(problems, "refusing to start: $HANDOFF_SOCKET is unset -- the loop always eventually runs a drift pass, which must hand its result to the publisher")
	}

	// ⚠️ REQUIRED FOR THE LOOP, OPTIONAL FOR `truss apply` (which never
	// reads it at all). A daemon nobody can scrape is the fail-open-with-a-
	// receipt shape observability/README.md exists to refuse: every other
	// refusal here stops a manifest that forgot something before it can run
	// silently; this is the same rule applied to its own observability.
	metricsListen := getenv("METRICS_LISTEN")
	if metricsListen == "" {
		problems = append(problems, "refusing to start: $METRICS_LISTEN is unset -- the loop must be scrapeable, not just running")
	}

	if len(problems) > 0 {
		return loopConfig{}, problems
	}
	return loopConfig{
		interval:          interval,
		driftAt:           driftAt,
		driftHeartbeatKey: driftHeartbeatKey,
		handoffSocket:     handoffSocket,
		metricsListen:     metricsListen,
	}, nil
}

// seedLastDrift reads the drift heartbeat once, at startup, so a restart
// does not re-run a drift pass that already ran today -- in memory alone,
// lastDrift would be zero on every restart, and a restart after the
// window opened would then rotate credentials on every crash-loop
// iteration. An absent or unparseable record seeds the zero time, which
// makes the first window this loop sees due -- correct on a virgin
// deployment, and the only case where guessing would be worse than
// acting. Errors are swallowed to that same zero-time default: a ledger
// this cannot yet reach is buildPass's refusal to report, not this
// function's.
func seedLastDrift(ctx context.Context, cfg config.Config, driftHeartbeatKey string) time.Time {
	store, err := buildLedgerStore(cfg)
	if err != nil {
		return time.Time{}
	}
	body, err := store.Get(ctx, driftHeartbeatKey)
	if err != nil {
		return time.Time{}
	}
	var hb ledger.Heartbeat
	if err := json.Unmarshal(body, &hb); err != nil {
		return time.Time{}
	}
	t, err := time.Parse(heartbeatTimeLayout, hb.Time)
	if err != nil {
		return time.Time{}
	}
	return t
}

// loopTurn runs one pass and reports what happened. fatal means a
// construction refusal -- buildPass could not even assemble the pass's
// deps -- which stops the loop the same way it stops `truss apply`: there
// is nothing to recover from by ticking again. problems is the refusal
// text, valid only when fatal is true.
//
// Extracted as a func type, rather than inlined into the loop below, so
// the loop's own pacing -- run once immediately, then on the tick, stop
// when told to -- is testable without a working ledger, forge, Vault and
// git behind it. realLoopTurn is the only production implementation.
type loopTurn func(ctx context.Context) (notifyText string, fatal bool, problems []string)

// realLoopTurn closes over passEnv and lcfg and calls buildPass fresh on
// every invocation, so a rotated ledger credential, forge key or Telegram
// token takes effect on the very next pass rather than requiring a
// restart -- see buildPass's own doc for why each of its pieces must be
// rebuilt rather than hoisted.
//
// lastDrift is captured by the closure and advanced in place: each call
// decides drift for ITSELF from the current time, and if it ran a drift
// pass, the NEXT call sees today's window as already handled. Advanced
// when the pass RETURNS, not when the heartbeat write inside it succeeds
// -- a broken ledger costs one missed drift rather than a rotation storm
// on every tick until the ledger recovers.
// stop, when non-nil, is wired to applyDeps.Stop on the deps buildPass
// returns -- the seam that lets a pass in flight finish its current unit
// and return rather than being cancelled through ctx (which would reach
// exec.CommandContext and SIGKILL a running tofu, orphaning its state
// lock; see internal/childproc's doc comment for the measurement).
func realLoopTurn(e passEnv, lcfg loopConfig, lastDrift time.Time, now func() time.Time, stop func() bool, snap *snapshots) loopTurn {
	return func(ctx context.Context) (string, bool, []string) {
		n := now()
		drift := lcfg.driftAt.due(n, lastDrift)

		deps, last, problems := buildPass(ctx, e, drift, lcfg.handoffSocket, lcfg.driftHeartbeatKey)
		if len(problems) > 0 {
			return "", true, problems
		}
		deps.Stop = stop
		deps.Record = snap.record
		snap.setInFlight(true)
		result := runApplyPass(ctx, deps, last)
		snap.setInFlight(false)
		if drift {
			lastDrift = n
		}
		return result.notifyText, false, nil
	}
}

// runLoop calls turn once immediately and then on every tick, until ctx is
// done, stopping reports true, or turn reports fatal.
//
// stopping is consulted right after turn returns, not before it is called
// or during it: a signal that arrives mid-pass is applyDeps.Stop's job
// (deps.Stop, wired by realLoopTurn to the same underlying flag) to let
// the in-flight unit finish; this check is what stops the LOOP itself from
// starting another turn once that one has returned. It is checked before
// the immediate-or-ticked wait, not folded into the select below, so a
// stop requested while the loop is between iterations is noticed without
// waiting for the next tick.
//
// ⚠️ RUN-NOW IS STILL MISSING. Each lands as its own change so every one
// can be watched passing, and failing, on its own.
func runLoop(ctx context.Context, interval time.Duration, turn loopTurn, stopping func() bool, stdout, stderr io.Writer) int {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		text, fatal, problems := turn(ctx)
		if fatal {
			for _, p := range problems {
				fmt.Fprintln(stderr, p)
			}
			return 1
		}
		if text != "" {
			fmt.Fprintln(stdout, text)
		}

		if stopping() {
			return 0
		}

		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// cmdLoop runs the same pass `truss apply` runs, once immediately and then
// on an interval, until ctx is done. passEnv and loopConfig are loaded
// ONCE, at startup; lastDrift is seeded once from the ledger so a restart
// does not re-run today's drift.
func cmdLoop(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss loop")
		return 2
	}

	e, problems := loadPassEnv(getenv, stderr)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	lcfg, lproblems := loadLoopConfig(getenv)
	if len(lproblems) > 0 {
		for _, p := range lproblems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	lastDrift := seedLastDrift(ctx, e.Cfg, lcfg.driftHeartbeatKey)

	snap := newSnapshots(time.Now())
	metricsSrv := newMetricsServer(snap)
	if err := listenMetrics(ctx, metricsSrv, lcfg.metricsListen); err != nil {
		fmt.Fprintln(stderr, "refusing to start: "+err.Error())
		return 1
	}
	// Provisional: an immediate Close rather than a graceful Shutdown with
	// a bounded grace period. The control listener (not yet built) needs
	// its own shutdown ordering relative to this one -- the lease, once it
	// exists, must not be released before both listeners are down -- so
	// this is revisited in that change rather than half-designed here.
	defer metricsSrv.Close()

	stopping, stop := armStopSignal(ctx, stderr)
	defer stop()
	turn := realLoopTurn(e, lcfg, lastDrift, time.Now, stopping, snap)

	return runLoop(ctx, lcfg.interval, turn, stopping, stdout, stderr)
}

// armStopSignal installs the loop's only signal handler and returns a
// stopping func reporting whether it has fired, plus a cleanup to call
// when the loop returns for any other reason.
//
// ⚠️ signal.Notify, NEVER signal.NotifyContext. NotifyContext cancels a
// context, and every external command this binary runs is built with
// childproc.Command around exec.CommandContext -- whose Cancel this
// package sets to SIGINT the child's process group, but only because
// nothing UPSTREAM of that already killed it. Cancelling ctx here would
// reach a running `tofu apply` through code that was never written to
// expect it, mid-unit, which is the exact leaked-lock failure this change
// exists to prevent. The signal sets a flag; nothing here cancels a
// context.
//
// Extracted from cmdLoop so the signal-to-flag wiring is testable with a
// real SIGTERM against a real process, without needing a working ledger,
// forge, Vault and git behind it.
func armStopSignal(ctx context.Context, stderr io.Writer) (stopping func() bool, cleanup func()) {
	var stopped atomic.Bool
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		// Reads ONCE: signal.Notify drops a second signal on this full
		// channel while nobody is reading, which is deliberate -- there is
		// one way to stop, and "I mean it, right now" is SIGKILL from
		// whatever supervises this process, which is not ours to design.
		// The ctx.Done() branch exists only so this goroutine does not
		// outlive a caller that returned for some other reason (a fatal
		// turn, a cancelled ctx) without ever receiving a signal -- in
		// production ctx never cancels and this branch never fires.
		select {
		case s := <-sig:
			fmt.Fprintf(stderr, "time=%s level=info msg=%s\n",
				time.Now().UTC().Format("15:04:05"),
				strconv.Quote(fmt.Sprintf("stopping: %s received; no new unit will start", s)))
			stopped.Store(true)
		case <-ctx.Done():
		}
	}()
	return stopped.Load, func() { signal.Stop(sig) }
}
