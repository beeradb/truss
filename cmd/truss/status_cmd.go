package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
)

// heartbeatTimeLayout is the heartbeat's own timestamp format --
// `date -u +%Y-%m-%dT%H:%M:%SZ` in the bash, and the Go port's
// time.Now().UTC().Format("2006-01-02T15:04:05Z") in apply_cmd.go, which
// must agree with ledger.Journal's own failedAtLayout for the same
// reason: `truss status` has to parse exactly what `truss apply` writes,
// not a superset of it, or a real heartbeat could fail to parse here while
// reading fine everywhere else.
const heartbeatTimeLayout = "2006-01-02T15:04:05Z"

// missedPasses is how many passes may go by before the applier is
// presumed stopped rather than merely quiet: long enough that one slow
// pass (a large plan, a busy state lock) does not trip a false alarm,
// short enough that a stopped applier is noticed well within the working
// day. It is the number the old 15-minute constant encoded -- three
// misses of a five-minute CronJob -- restated as the arithmetic instead
// of the answer, so the cadence and the threshold cannot independently
// drift out of agreement the way they did once already (see loop.go's
// own history: a naive carry-over of "three misses" at the loop's
// 1-minute interval would have paged daily).
const missedPasses = 3

// staleAfter is the ONE definition of "too long since the last pass",
// shared by `truss status` and the loop's own /metrics and control API:
// there is no second number anywhere in this binary that also means
// this. Under `truss apply` (a one-shot process with no interval of its
// own) `truss status` assumes defaultLoopInterval (loop.go), which is
// the schedule any deployment of this binary is expected to run under.
func staleAfter(interval time.Duration) time.Duration { return missedPasses * interval }

// defaultStaleAfter is `truss status`'s own default when --stale-after is
// not given: three missed passes at the loop's own default interval.
var defaultStaleAfter = staleAfter(defaultLoopInterval)

// cmdStatus implements `truss status` (docs/work-items.md:86-133): read
// the ledger over S3, the same credential the applier uses, and answer
// "what is the applier doing" from a laptop -- never through kubectl,
// never through the forge, never through git.
//
// ⚠️ THE STALENESS CHECK IS THE WHOLE POINT OF THIS COMMAND, AND IT IS
// EASY TO WRITE A VERSION THAT SKIPS IT. Heartbeat.Failure and the rest of
// the record describe the outcome of the LAST pass that ran, and that
// record stays readable forever whether the CronJob completed five
// minutes ago or five days ago -- the same trap observability/README.md
// documents for the Pushgateway, where a scrape keeps re-serving the last
// pushed value long after the job that pushed it has stopped existing. A
// status command that reads only the outcome fields would report a dead
// applier as healthy for as long as its last real pass happened to look
// clean. Comparing the heartbeat's own timestamp against wall-clock time,
// below, is the only thing in this command that can tell "quiet" apart
// from "gone".
func cmdStatus(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	staleAfter, usageErr := parseStatusArgs(args)
	if usageErr != "" {
		fmt.Fprintln(stderr, usageErr)
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 2
	}
	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	journal := &ledger.Journal{Store: store, Layout: layoutFor(cfg)}

	head, err := journal.Head(ctx)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintf(stderr, "status: no %s in the ledger -- nothing has ever run\n", cfg.LedgerHeadKey)
		} else {
			fmt.Fprintf(stderr, "status: could not read HEAD: %v\n", err)
		}
		return 2
	}

	hbBody, err := store.Get(ctx, cfg.HeartbeatKey)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintf(stderr, "status: no %s in the ledger -- the applier has never completed a pass\n", cfg.HeartbeatKey)
		} else {
			fmt.Fprintf(stderr, "status: could not read the heartbeat: %v\n", err)
		}
		return 2
	}
	var hb ledger.Heartbeat
	if err := json.Unmarshal(hbBody, &hb); err != nil {
		fmt.Fprintf(stderr, "status: could not parse the heartbeat: %v\n", err)
		return 2
	}
	writtenAt, err := time.Parse(heartbeatTimeLayout, hb.Time)
	if err != nil {
		fmt.Fprintf(stderr, "status: heartbeat time %q does not match the applier's own timestamp format: %v\n", hb.Time, err)
		return 2
	}

	age := time.Since(writtenAt).Round(time.Second)
	stale := age > staleAfter

	fmt.Fprintf(stdout, "HEAD:            %s\n", head)
	fmt.Fprintf(stdout, "heartbeat sha:   %s\n", hb.LastSHA)
	fmt.Fprintf(stdout, "heartbeat time:  %s (%s ago)\n", hb.Time, age)
	fmt.Fprintf(stdout, "applied:         %d\n", hb.Applied)
	fmt.Fprintf(stdout, "noop:            %d\n", hb.Noop)
	if hb.Failure != nil {
		fmt.Fprintf(stdout, "failure:         %s\n", *hb.Failure)
	} else {
		fmt.Fprintln(stdout, "failure:         none")
	}
	if stale {
		fmt.Fprintf(stdout, "STALE:           heartbeat is %s old, more than --stale-after %s -- the applier may have stopped running\n", age, staleAfter)
	}
	for _, e := range hb.Expiring {
		if e.DaysLeft == nil {
			fmt.Fprintf(stdout, "expiring:        %s (no expiry recorded)\n", e.Name)
		} else {
			fmt.Fprintf(stdout, "expiring:        %s (%d days left)\n", e.Name, *e.DaysLeft)
		}
	}

	if hb.Failure != nil || stale {
		return 1
	}
	return 0
}

// parseStatusArgs reads the optional "--stale-after <duration>" flag,
// defaulting to defaultStaleAfter. An unparseable duration is refused
// outright (exit 2, via the empty return here turning into cmdStatus's own
// message) -- there is no fallback to the default, because silently
// substituting a duration nobody asked for is exactly the kind of guess
// this whole command exists to avoid making about the applier's health.
func parseStatusArgs(args []string) (staleAfter time.Duration, usageErr string) {
	const usage = "usage: truss status [--stale-after <duration>]"
	staleAfter = defaultStaleAfter
	for i := 0; i < len(args); i++ {
		if args[i] != "--stale-after" {
			return 0, usage
		}
		if i+1 >= len(args) {
			return 0, usage
		}
		d, err := time.ParseDuration(args[i+1])
		if err != nil {
			return 0, fmt.Sprintf("status: --stale-after %q: %v", args[i+1], err)
		}
		staleAfter = d
		i++
	}
	return staleAfter, ""
}
