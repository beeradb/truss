package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
)

// cmdWhy implements `truss why <sha>` (docs/work-items.md:86-133): explain
// what happened to one commit in the queue, reading only the ledger --
// never kubectl, never the forge, never git.
//
// Exit-code contract, deliberately the same three-way `ledger get` already
// uses (§4.9): 0 a record was found and printed, 2 no record under either
// key ("the queue has not reached this commit" -- information, not an
// error), 1 anything else (a config problem, an unreachable ledger, or a
// record that exists but will not parse).
func cmdWhy(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: truss why <sha>")
		return 2
	}
	sha := args[0]

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	layout := layoutFor(cfg)

	appliedBody, appliedErr := store.Get(ctx, layout.AppliedKey(sha))
	if appliedErr == nil {
		return printWhyApplied(sha, appliedBody, stdout, stderr)
	}
	if !errors.Is(appliedErr, ledger.ErrNotFound) {
		fmt.Fprintf(stderr, "why: could not read the applied record for %s: %v\n", sha, appliedErr)
		return 1
	}

	failedBody, failedErr := store.Get(ctx, layout.FailedKey(sha))
	if failedErr == nil {
		return printWhyFailed(sha, failedBody, stdout, stderr)
	}
	if !errors.Is(failedErr, ledger.ErrNotFound) {
		fmt.Fprintf(stderr, "why: could not read the failed record for %s: %v\n", sha, failedErr)
		return 1
	}

	// Neither applied/<sha> nor failed/<sha> exists: the queue simply has
	// not reached this commit yet. That is information about where the
	// queue is, not a failure of this command, so it gets the same exit
	// code `ledger get` already uses for "absent" (§4.9).
	fmt.Fprintf(stderr, "why: no record for %s under %s or %s -- the queue has not reached this commit\n", sha, layout.AppliedPrefix, layout.FailedPrefix)
	return 2
}

// printWhyApplied decodes applied/<sha>'s body, which is one of three
// shapes truss itself ever writes there -- a normal apply (`{"roots":...}`,
// ledger.RootSummary per root), a noop (`{"noop":true}`), or a skip
// (`{"skipped":true,"reason":...,"at":...}`, written by `truss skip`) --
// and prints whichever it finds.
func printWhyApplied(sha string, body []byte, stdout, stderr io.Writer) int {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		fmt.Fprintf(stderr, "why: could not parse the applied record for %s: %v\n", sha, err)
		return 1
	}

	if _, ok := probe["skipped"]; ok {
		var rec struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		}
		if err := json.Unmarshal(body, &rec); err != nil {
			fmt.Fprintf(stderr, "why: could not parse the skipped record for %s: %v\n", sha, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: skipped\n", sha)
		fmt.Fprintf(stdout, "  reason: %s\n", rec.Reason)
		fmt.Fprintf(stdout, "  at:     %s\n", rec.At)
		return 0
	}

	if _, ok := probe["noop"]; ok {
		fmt.Fprintf(stdout, "%s: applied, no changes (noop)\n", sha)
		return 0
	}

	if rootsRaw, ok := probe["roots"]; ok {
		var roots map[string]ledger.RootSummary
		if err := json.Unmarshal(rootsRaw, &roots); err != nil {
			fmt.Fprintf(stderr, "why: could not parse the applied record's roots for %s: %v\n", sha, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: applied\n", sha)
		names := make([]string, 0, len(roots))
		for name := range roots {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rs := roots[name]
			// A nil ResourceChanges is summary_from_plan's own "could not
			// parse `tofu show -json`" fallback (ledger.RootSummary's doc) --
			// it must print as unknown, never as 0, because 0 means "plan
			// had no changes" and nil means "nobody knows what the plan had".
			if rs.ResourceChanges == nil {
				fmt.Fprintf(stdout, "  %s: unknown resource changes\n", name)
			} else {
				fmt.Fprintf(stdout, "  %s: %d resource change(s)\n", name, *rs.ResourceChanges)
			}
		}
		return 0
	}

	fmt.Fprintf(stderr, "why: applied record for %s has none of the shapes truss writes there (skipped, noop, roots)\n", sha)
	return 1
}

// printWhyFailed decodes failed/<sha>'s body -- reason then at, matching
// ledger.Journal.PutFailed -- and prints it.
func printWhyFailed(sha string, body []byte, stdout, stderr io.Writer) int {
	var rec struct {
		Reason string `json:"reason"`
		At     string `json:"at"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		fmt.Fprintf(stderr, "why: could not parse the failed record for %s: %v\n", sha, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: failed\n", sha)
	fmt.Fprintf(stdout, "  reason: %s\n", rec.Reason)
	fmt.Fprintf(stdout, "  at:     %s\n", rec.At)
	return 0
}
