package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
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

	// Guard 1: --reason is mandatory and must be non-empty after trimming
	// space. No default, no prompt -- a skip with nothing stated about why
	// is exactly the shortcut work-items.md warns against, and it needs no
	// ledger access to check, so it is refused before anything else is even
	// attempted.
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		fmt.Fprintln(stderr, "skip: refusing -- --reason is required and must be non-empty. State, in the reason, why this commit's plan cannot ever apply, then run the command again with that text.")
		return 1
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

	// Guard 2: failed/<sha> must exist. The queue stops AT the commit it
	// fails on (§2 item 6), so a failed/ record is the proof the applier
	// actually reached and tried this commit. Without one, "skip" would be
	// guessing about work nobody attempted -- the exact failure mode
	// work-items.md names as "refuse a commit whose plan the applier never
	// tried".
	_, err = store.Get(ctx, journal.Layout.FailedKey(sha))
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintf(stderr, "skip: refusing -- no failed record for %s. The applier never tried this commit, so there is nothing proven to skip past. If it needs skipping, wait until the applier reaches it and fails there first, then run this again.\n", sha)
			return 1
		}
		fmt.Fprintf(stderr, "skip: could not read the failed record for %s: %v\n", sha, err)
		return 2
	}

	// Guard 3: sha must not already be HEAD -- there is nothing to advance
	// past if the queue is already there.
	head, err := journal.Head(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "skip: could not read HEAD: %v\n", err)
		return 2
	}
	if head == sha {
		fmt.Fprintf(stderr, "skip: refusing -- HEAD is already %s. There is nothing to advance past; if the queue still looks stuck, something else is wrong and `truss status` is the next step.\n", sha)
		return 1
	}

	// Guard 4: confirm by naming. TRUSS_SKIP_I_UNDERSTAND must equal this
	// exact sha, not merely be set -- see envSkipConfirm's doc.
	if getenv(envSkipConfirm) != sha {
		fmt.Fprintf(stderr, "skip: refusing -- set %s=%s to confirm you mean to skip exactly this commit.\n", envSkipConfirm, sha)
		return 1
	}

	// Write order: the record first, then HEAD. A crash between the two
	// leaves an explained commit behind (applied/<sha> already says
	// skipped) rather than an unexplained jump (HEAD past a commit with no
	// record of why at all).
	// ⚠️ THE ANNOUNCEMENT COMES BEFORE THE SKIP, AND A SKIP THAT CANNOT BE
	// ANNOUNCED DOES NOT HAPPEN.
	//
	// docs/threat-model.md says of the one existing escape hatch that an
	// operator "can never do it quietly, which is the other point", and
	// docs/operations.md calls it the only one. This command is a second
	// escape hatch and a narrower one -- it is aimed at a single commit,
	// where turning protection off is all-or-nothing -- so the quietness
	// property has to be earned rather than inherited.
	//
	// Sending first is what earns it. If the alert goes out and the skip
	// then fails, somebody investigates a skip that did not happen, which
	// costs a minute. If the skip were performed first and the alert failed,
	// a commit the applier refused would have been walked past with nothing
	// anywhere saying so -- and the ledger record alone does not count,
	// because nothing reads it unless a person already suspects something.
	dir := secrets.Dir{Root: cfg.SecretsDir}
	tg, err := loadTelegram(dir, getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "skip: could not load the alert credentials: %v\n", err)
		return 2
	}
	announce := fmt.Sprintf("%s: SKIPPED %s by hand -- %s", defaultAlertSubject, sha, trimmedReason)
	if err := tg.Send(ctx, announce); err != nil {
		fmt.Fprintf(stderr, "skip: refusing -- could not announce the skip (%v). "+
			"A skip nobody is told about is the one thing this command must not be, so nothing has been written. "+
			"Fix the alert transport and run it again.\n", err)
		return 1
	}

	if err := journal.PutSkipped(ctx, sha, trimmedReason); err != nil {
		fmt.Fprintf(stderr, "skip: could not write the skipped record for %s: %v\n", sha, err)
		return 2
	}
	if err := journal.AdvanceHead(ctx, sha); err != nil {
		fmt.Fprintf(stderr, "skip: wrote the skipped record for %s but could not advance HEAD: %v. The record is in place; re-run once the ledger is reachable to finish advancing HEAD.\n", sha, err)
		return 2
	}

	fmt.Fprintf(stdout, "skipped %s: %s\n", sha, trimmedReason)
	return 0
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
