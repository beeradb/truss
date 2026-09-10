// Package notify composes the single-line status message a run reports, and
// (elsewhere, not in this file) sends it to Telegram.
package notify

import (
	"fmt"
	"strconv"
	"strings"
)

// Expiring is one credential the sweep found close to (or past, or with no
// recorded) expiry.
//
// ⚠️ This is a stand-in for secrets.Expiring (port-plan.md §4.7), which the
// spec for this package names directly (`Expiring []secrets.Expiring`).
// internal/secrets does not exist in this tree yet and building it is out of
// scope here, so Report carries this package's own copy of the same two
// fields instead of importing a package that cannot be built. Once
// internal/secrets lands, Report.Expiring should be retyped to
// []secrets.Expiring and this type deleted -- see the report handed back
// with this change.
type Expiring struct {
	Name     string
	DaysLeft *int
}

// Report is everything a run needs to describe itself in one line.
type Report struct {
	Subject string // "platform applier"
	// LastSHA is the ledger position: the last commit this pass left
	// applied. It is what the non-failure summary reports.
	//
	// ⚠️ IT IS NOT WHERE A FAILURE HAPPENED, AND SAYING SO WAS A REAL BUG.
	// The failure clause used to read "FAILED at <LastSHA>", which named
	// the last commit that SUCCEEDED while describing a refusal of the next
	// one. Observed on a live applier 2026-09-10: "FAILED at f42f97f"
	// about a provider block that exists only in the commit after it. Use
	// FailedSHA for that.
	LastSHA string

	// FailedSHA is the commit a failure belongs to. Empty when the failure
	// belongs to no commit -- a protection gate that refused before the
	// queue was read, a credential that would not mount, an expiry sweep
	// that could not report -- and then the alert names no commit at all,
	// which is the honest shape rather than a plausible wrong one.
	FailedSHA string

	// PlannedSHA is the branch head whose tree was checked out and planned
	// when FailedSHA failed. It is reported separately because the applier
	// plans AT THE HEAD while working through the queue one commit at a
	// time, so the tree that produced a tofu error is not in general the
	// tree of the commit whose turn it was. Omitted from the text when it
	// is empty or equal to FailedSHA, so the common single-commit case
	// stays short.
	PlannedSHA string

	Applied, Noop int
	Failure       string

	DriftRun     bool
	DriftSkipped string

	Drifted, Errored []string

	RotatedChanges int

	Expiring []Expiring

	// ExpiryUnavailable is why the expiry sweep could not report, when it
	// could not. It is deliberately NOT a Failure: the sweep refuses to
	// claim a clean bill it did not earn, and at cutover it cannot earn one
	// because nothing seeds `expires` into Vault yet. Promoting that into
	// Failure made every clean pass exit 1 and send FAILED, ~288 times a
	// day -- an alert channel nobody reads is where a real digest-gate
	// refusal goes to die, which is why both 2026-09-08 reviewers called
	// this a security cost rather than noise. The reference bash never set
	// failure for it either (check_credential_lifetimes is `|| true`
	// throughout).
	ExpiryUnavailable string
}

// Compose builds the exact text send_telegram (apply.sh:408-448) sends.
//
// The opening clause is one of three: a failure, an idle run, or a normal
// applied/noop summary. Five more clauses are appended, in this fixed order,
// each only when it has something to say: a skipped drift check, a rotation
// count, the names of drifted roots, the names of roots drift could not be
// checked for, and any credentials nearing (or past, or with no recorded)
// expiry. Clauses are joined with "; " and name lists within a clause are
// joined with ", ".
func Compose(r Report) string {
	var text string
	switch {
	case r.Failure != "" && r.FailedSHA != "":
		at := r.FailedSHA
		if r.PlannedSHA != "" && r.PlannedSHA != r.FailedSHA {
			at += " (planned at " + r.PlannedSHA + ")"
		}
		text = fmt.Sprintf("%s FAILED at %s: %s (applied=%d noop=%d)",
			r.Subject, at, trimReason(r.Failure), r.Applied, r.Noop)
	case r.Failure != "":
		// No commit to name. Naming one anyway is what the bash did and
		// what truss copied: it printed the ledger position under the word
		// "at", so a branch-protection refusal that never looked at a
		// commit still pointed a reader at one.
		text = fmt.Sprintf("%s FAILED: %s (applied=%d noop=%d)",
			r.Subject, trimReason(r.Failure), r.Applied, r.Noop)
	case r.Applied == 0 && r.Noop == 0:
		text = fmt.Sprintf("%s: nothing to apply", r.Subject)
	default:
		text = fmt.Sprintf("%s: applied=%d noop=%d last=%s", r.Subject, r.Applied, r.Noop, r.LastSHA)
	}

	if r.DriftRun && r.DriftSkipped != "" {
		text += "; DRIFT NOT CHECKED: " + r.DriftSkipped
	}
	if r.RotatedChanges != 0 {
		text += fmt.Sprintf("; rotated credentials (%d changes)", r.RotatedChanges)
	}
	if len(r.Drifted) > 0 {
		text += "; DRIFT: " + strings.Join(r.Drifted, ", ") + " differ from the code"
	}
	if len(r.Errored) > 0 {
		text += "; drift UNKNOWN for: " + strings.Join(r.Errored, ", ")
	}
	if r.ExpiryUnavailable != "" {
		text += "; EXPIRY NOT CHECKED: " + r.ExpiryUnavailable
	}
	if len(r.Expiring) > 0 {
		names := make([]string, len(r.Expiring))
		for i, e := range r.Expiring {
			if e.DaysLeft == nil {
				names[i] = e.Name + " (no expiry recorded)"
			} else {
				names[i] = e.Name + " in " + strconv.Itoa(*e.DaysLeft) + "d"
			}
		}
		text += "; EXPIRING: " + strings.Join(names, ", ")
	}

	return text
}

// Silent reports whether Compose has nothing to say about r at all: no
// failure, nothing applied, nothing no-opped, and not one of the appended
// clauses.
//
// ⚠️ IT IS NOT "NOTHING WAS APPLIED", AND THAT DISTINCTION IS THE WHOLE
// POINT. A caller that suppresses the message on an idle pass -- which is
// what a dead-man's-switch deployment does -- suppresses the DRIFT, EXPIRING,
// EXPIRY NOT CHECKED and rotation clauses with it if it asks the narrower
// question, because a drift pass applies nothing by definition. The daily
// pass is exactly the pass that carries those clauses and exactly the pass
// that looks idle, so asking "did anything apply" silences the one report
// that matters while the monitor stays green.
//
// It lives here, beside Compose, because the list of clauses is Compose's own
// and a second copy of it at a call site is how it drifts.
// TestSilentAgreesWithComposeOnEveryField holds the two together.
func (r Report) Silent() bool {
	if r.Failure != "" || r.Applied != 0 || r.Noop != 0 {
		return false
	}
	return !(r.DriftRun && r.DriftSkipped != "") &&
		r.RotatedChanges == 0 &&
		len(r.Drifted) == 0 &&
		len(r.Errored) == 0 &&
		r.ExpiryUnavailable == "" &&
		len(r.Expiring) == 0
}

// trimReason reproduces trim_reason (apply.sh:345-354): NUL bytes are
// dropped, the text is cut at 800 bytes, and a truncation marker is
// appended when the ORIGINAL text (before NUL-stripping) was over 800 bytes
// -- the same quirk the bash has, preserved rather than corrected.
//
// ⚠️ This duplicates ledger.TrimReason (port-plan.md §4.2), which does not
// exist in this tree yet. Once internal/ledger lands, Compose should take an
// already-trimmed Report.Failure (trimmed by whoever populates the Report,
// the same place PutFailed trims it) and this function should be deleted --
// see the report handed back with this change.
func trimReason(s string) string {
	stripped := strings.ReplaceAll(s, "\x00", "")
	first := stripped
	if len(first) > 800 {
		first = first[:800]
	}
	if len(s) > 800 {
		first += "\n... truncated; see the run's pod logs for the rest."
	}
	return first
}
