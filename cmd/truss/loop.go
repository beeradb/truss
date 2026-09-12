package main

import (
	"context"
	"fmt"
	"io"
	"time"
)

// defaultLoopInterval is the loop's pass interval when $LOOP_INTERVAL is
// unset -- a stated design decision (the loop-mode work), not a guess.
const defaultLoopInterval = time.Minute

// loopConfig is `truss loop`'s own environment, layered on top of passEnv
// (apply_cmd.go), which it shares with `truss apply`.
type loopConfig struct {
	interval time.Duration
}

// loadLoopConfig reads and validates the loop's own environment on top of
// whatever loadPassEnv already validated. Fail-closed, matching
// config.Load's own contract: every problem reported, nothing defaulted
// that matters.
//
// ⚠️ THIS IS NOT YET THE FULL LOOP CONFIGURATION. Drift scheduling and the
// publisher's socket are validated where they start being used, not here in
// advance of any code that would act on them -- a refusal that gates
// nothing is not a refusal, it is a trap for whoever configures it first.
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

	if len(problems) > 0 {
		return loopConfig{}, problems
	}
	return loopConfig{interval: interval}, nil
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

// realLoopTurn closes over passEnv and calls buildPass fresh on every
// invocation, so a rotated ledger credential, forge key or Telegram token
// takes effect on the very next pass rather than requiring a restart -- see
// buildPass's own doc for why each of its pieces must be rebuilt rather
// than hoisted.
func realLoopTurn(e passEnv) loopTurn {
	return func(ctx context.Context) (string, bool, []string) {
		deps, last, problems := buildPass(ctx, e)
		if len(problems) > 0 {
			return "", true, problems
		}
		result := runApplyPass(ctx, deps, last)
		return result.notifyText, false, nil
	}
}

// runLoop calls turn once immediately and then on every tick, until ctx is
// done or turn reports fatal. It is `cmdLoop`'s body with the pass
// construction factored out, purely so it can be driven by a fake turn in
// tests.
//
// ⚠️ THIS IS THE FREQUENT-PASS-ONLY SHAPE, THE FIRST OF SEVERAL STEPS. No
// drift scheduling, no run-now, no graceful stop yet -- ctx.Done() is the
// only way this returns before a fatal turn, and nothing today ever
// cancels the ctx that reaches here (run.go's context.Background() is
// never cancelled). Each is landed as its own change so every one of them
// can be watched passing, and failing, on its own.
func runLoop(ctx context.Context, interval time.Duration, turn loopTurn, stdout, stderr io.Writer) int {
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

		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// cmdLoop runs the same pass `truss apply` runs, once immediately and then
// on an interval, until ctx is done. passEnv is loaded ONCE, at startup.
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

	return runLoop(ctx, lcfg.interval, realLoopTurn(e), stdout, stderr)
}
