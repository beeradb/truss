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
	LastSHA string

	Applied, Noop int
	Failure       string

	DriftRun     bool
	DriftSkipped string

	Drifted, Errored []string

	RotatedChanges int

	Expiring []Expiring
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
	case r.Failure != "":
		text = fmt.Sprintf("%s FAILED at %s: %s (applied=%d noop=%d)",
			r.Subject, r.LastSHA, trimReason(r.Failure), r.Applied, r.Noop)
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
