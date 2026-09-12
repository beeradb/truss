package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/secrets"
)

// envSkipConfirm is the confirm-by-naming environment variable, following
// scripts/ledger-retention's `lock` idiom exactly: a flag is something a
// script can pass reflexively on every invocation, but typing the sha into
// an environment variable by hand is a deliberate act a script blindly
// retrying cannot forge by accident.
const envSkipConfirm = "TRUSS_SKIP_I_UNDERSTAND"

// cmdSkip implements `truss skip <sha> --reason <text>`
// (docs/work-items.md:86-133).
//
// ⚠️ THIS IS THE DANGEROUS ONE, DELIBERATELY MADE THE HARDEST. Advancing
// HEAD past a commit is editing the applier's memory of what it has done,
// by hand, out of band -- exactly the operation that, made convenient,
// gets reached for INSTEAD of understanding a failure. Four guards below
// are all required and all checked -- and refuse -- before anything is
// written. Each is independently testable (skip_cmd_test.go has one test
// per guard) because a guard nobody has watched fail is a claim, not a
// guard.
//
// Exit codes: 0 skipped, 1 a guard refused (message names which one and
// what to do about it), 2 could not ask (bad usage, config problems, the
// ledger unreachable).
func cmdSkip(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	sha, reason, usageErr := parseSkipArgs(args)
	if usageErr != "" {
		fmt.Fprintln(stderr, usageErr)
		return 2
	}

	trimmedReason := strings.TrimSpace(reason)

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

	// Guards 1-3: a non-empty reason, a failed/<sha> record, sha not
	// already HEAD -- none of them touch Telegram, and neither did the
	// original ordering here: a pass that fails one of these must not also
	// need a working alert credential to say so.
	if outcome := checkSkipGuards(ctx, journal, sha, trimmedReason); outcome.Refused != "" {
		fmt.Fprintln(stderr, "skip: "+outcome.Refused)
		if outcome.NeedsLedger {
			return 2
		}
		return 1
	}

	// Guard 4: confirm by naming. TRUSS_SKIP_I_UNDERSTAND must equal this
	// exact sha, not merely be set -- see envSkipConfirm's doc. This is
	// the CLI's own confirmation; the control API (control_server.go) has
	// no environment to type into and uses expect_head instead.
	if getenv(envSkipConfirm) != sha {
		fmt.Fprintf(stderr, "skip: refusing -- set %s=%s to confirm you mean to skip exactly this commit.\n", envSkipConfirm, sha)
		return 1
	}

	dir := secrets.Dir{Root: cfg.SecretsDir}
	tg, err := loadTelegram(dir, getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "skip: could not load the alert credentials: %v\n", err)
		return 2
	}

	outcome := finishSkip(ctx, journal, tg, sha, trimmedReason)
	if outcome.Refused != "" {
		fmt.Fprintln(stderr, "skip: "+outcome.Refused)
		if outcome.NeedsLedger {
			return 2
		}
		return 1
	}

	fmt.Fprintf(stdout, "skipped %s: %s\n", sha, trimmedReason)
	return 0
}

// skipOutcome is what checkSkipGuards or finishSkip did or refused.
type skipOutcome struct {
	// Refused, when non-empty, is the guard's own refusal sentence -- a
	// caller prints or returns it verbatim, never re-derives it, so the
	// CLI and the control API cannot end up saying two different things
	// about the same guard.
	Refused string
	// NeedsLedger is true when Refused came from being unable to reach the
	// ledger at all (config, network, a store error), as opposed to a
	// guard that read it fine and said no. Distinguishes "could not ask"
	// from "asked and was refused" -- exit 2 vs. 1 for the CLI, 400/503
	// vs. 422 for the control API.
	NeedsLedger bool
}

// checkSkipGuards runs skip's three ledger-checkable guards: a non-empty
// reason, a failed/<sha> record proving the applier actually tried this
// commit, and sha not already being HEAD. It touches no Telegram
// credential and writes nothing, on purpose: a guard refusal here must not
// ALSO require a working alert transport to report, and finishSkip (below)
// is the only thing in this file allowed to write.
//
// It does NOT check the fourth guard (confirm by naming): that is
// specific to each CALLER -- the CLI's environment variable (cmdSkip,
// above) or the control API's expect_head compare-and-swap
// (control_server.go) -- checked by the caller in whatever order its own
// contract requires.
//
// ⚠️ EXTRACTED SO THERE IS EXACTLY ONE IMPLEMENTATION OF THE DANGEROUS
// PART. Two copies of "which guards, in which order, saying what" is the
// internal/plan/digest.go hazard AGENTS.md already names: they drift, and
// the reviewer of one PR does not see it happen in the other.
func checkSkipGuards(ctx context.Context, journal *ledger.Journal, sha, trimmedReason string) skipOutcome {
	// Guard 1: reason must be non-empty after trimming space. No default,
	// no prompt -- needs no ledger access, so it is refused before
	// anything else is even attempted.
	if trimmedReason == "" {
		return skipOutcome{Refused: "refusing -- reason is required and must be non-empty. State, in the reason, why this commit's plan cannot ever apply, then try again with that text."}
	}

	// Guard 2: failed/<sha> must exist. The queue stops AT the commit it
	// fails on (§2 item 6), so a failed/ record is the proof the applier
	// actually reached and tried this commit. Without one, a skip would be
	// guessing about work nobody attempted.
	_, err := journal.Store.Get(ctx, journal.Layout.FailedKey(sha))
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			return skipOutcome{Refused: fmt.Sprintf("refusing -- no failed record for %s. The applier never tried this commit, so there is nothing proven to skip past. If it needs skipping, wait until the applier reaches it and fails there first, then try again.", sha)}
		}
		return skipOutcome{Refused: fmt.Sprintf("could not read the failed record for %s: %v", sha, err), NeedsLedger: true}
	}

	// Guard 3: sha must not already be HEAD -- there is nothing to advance
	// past if the queue is already there.
	head, err := journal.Head(ctx)
	if err != nil {
		return skipOutcome{Refused: fmt.Sprintf("could not read HEAD: %v", err), NeedsLedger: true}
	}
	if head == sha {
		return skipOutcome{Refused: fmt.Sprintf("refusing -- HEAD is already %s. There is nothing to advance past; if the queue still looks stuck, something else is wrong and `truss status` is the next step.", sha)}
	}
	return skipOutcome{}
}

// finishSkip announces the skip to Telegram BEFORE writing anything, and
// on success writes PutSkipped then AdvanceHead in that order. Callers
// must have already run checkSkipGuards (and their own fourth guard)
// successfully; finishSkip does not re-check any of them.
//
// Write order: the record first, then HEAD. A crash between the two
// leaves an explained commit behind (applied/<sha> already says skipped)
// rather than an unexplained jump. THE ANNOUNCEMENT COMES BEFORE THE
// SKIP, AND A SKIP THAT CANNOT BE ANNOUNCED DOES NOT HAPPEN --
// docs/threat-model.md says of the one existing escape hatch that an
// operator "can never do it quietly, which is the other point", and this
// is a second, narrower one, so the quietness property has to be earned
// rather than inherited. Sending first costs a minute of confusion if the
// skip then fails; sending after would let a commit get walked past with
// nothing anywhere saying so the moment the alert itself failed.
func finishSkip(ctx context.Context, journal *ledger.Journal, tg notify.Telegram, sha, trimmedReason string) skipOutcome {
	announce := fmt.Sprintf("%s: SKIPPED %s by hand -- %s", defaultAlertSubject, sha, trimmedReason)
	if err := tg.Send(ctx, announce); err != nil {
		return skipOutcome{Refused: fmt.Sprintf("refusing -- could not announce the skip (%v). "+
			"A skip nobody is told about is the one thing this command must not be, so nothing has been written. "+
			"Fix the alert transport and try again.", err)}
	}

	if err := journal.PutSkipped(ctx, sha, trimmedReason); err != nil {
		return skipOutcome{Refused: fmt.Sprintf("could not write the skipped record for %s: %v", sha, err), NeedsLedger: true}
	}
	if err := journal.AdvanceHead(ctx, sha); err != nil {
		return skipOutcome{Refused: fmt.Sprintf("wrote the skipped record for %s but could not advance HEAD: %v. The record is in place; try again once the ledger is reachable to finish advancing HEAD.", sha, err), NeedsLedger: true}
	}
	return skipOutcome{}
}

// parseSkipArgs pulls the sha and the --reason value out of args in any
// relative order. usageErr is non-empty only for a genuinely malformed
// invocation (wrong number of positional arguments, or a --reason with
// nothing after it) -- --reason simply being ABSENT is not malformed usage,
// it is guard 1's job to catch, so that "missing --reason" and "empty
// --reason" refuse identically as the same guard.
func parseSkipArgs(args []string) (sha, reason string, usageErr string) {
	const usage = "usage: truss skip <sha> --reason <text>"
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--reason" {
			if i+1 >= len(args) {
				return "", "", usage
			}
			reason = args[i+1]
			i++
			continue
		}
		positional = append(positional, args[i])
	}
	if len(positional) != 1 {
		return "", "", usage
	}
	return positional[0], reason, ""
}
