package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/secrets"
)

// defaultAlertSubject is what every message this binary sends is addressed
// from.
//
// notify.Report's own doc explains why it is a field there rather than a
// literal: "this repository is the shareable engine; cmd/truss defaults it so
// the emitted text is unchanged." It is a constant HERE because three callers
// need the same word -- the pass, this command, and `truss skip` -- and a
// fact stated in a third place is a fact that will disagree with itself.
//
// Decision 8 (docs/port-plan.md) makes this configuration eventually,
// defaulting to today's value. A constant is what a default looks like before
// the knob exists.
const defaultAlertSubject = "platform applier"

// expiringDTO and reportDTO are the JSON wire shape `truss notify` reads on
// stdin. notify.Report and notify.Expiring carry no JSON tags of their own
// (they are composed in-process by the apply pass; notify's own doc records
// that Report.Expiring is a stand-in for secrets.Expiring, retyped once
// that package exists to import), so this is the one conversion layer
// between "a JSON report" and the package's actual types.
type expiringDTO struct {
	Name     string `json:"name"`
	DaysLeft *int   `json:"days_left"`
}

type reportDTO struct {
	Subject string `json:"subject"`
	LastSHA string `json:"last_sha"`
	// failed_sha and planned_sha travel beside last_sha rather than
	// replacing it: last_sha is the ledger position and still what a
	// non-failure summary reports, while these two say where a failure
	// actually was. Absent means "this failure belongs to no commit",
	// which is what a caller composing a report about a protection
	// refusal should send -- see notify.Report.FailedSHA.
	FailedSHA  string `json:"failed_sha"`
	PlannedSHA string `json:"planned_sha"`

	Applied int    `json:"applied"`
	Noop    int    `json:"noop"`
	Failure string `json:"failure"`

	DriftRun     bool   `json:"drift_run"`
	DriftSkipped string `json:"drift_skipped"`

	Drifted []string `json:"drifted"`
	Errored []string `json:"errored"`

	RotatedChanges int `json:"rotated_changes"`

	Expiring []expiringDTO `json:"expiring"`
}

func (d reportDTO) toReport() notify.Report {
	expiring := make([]notify.Expiring, len(d.Expiring))
	for i, e := range d.Expiring {
		expiring[i] = notify.Expiring{Name: e.Name, DaysLeft: e.DaysLeft}
	}
	subject := d.Subject
	if subject == "" {
		subject = defaultAlertSubject
	}
	return notify.Report{
		Subject:        subject,
		LastSHA:        d.LastSHA,
		FailedSHA:      d.FailedSHA,
		PlannedSHA:     d.PlannedSHA,
		Applied:        d.Applied,
		Noop:           d.Noop,
		Failure:        d.Failure,
		DriftRun:       d.DriftRun,
		DriftSkipped:   d.DriftSkipped,
		Drifted:        d.Drifted,
		Errored:        d.Errored,
		RotatedChanges: d.RotatedChanges,
		Expiring:       expiring,
	}
}

// toNotifyExpiring converts the sweep's own findings into notify's copy of
// the same two fields (see reportDTO's doc comment on why two types exist).
func toNotifyExpiring(in []secrets.Expiring) []notify.Expiring {
	out := make([]notify.Expiring, len(in))
	for i, e := range in {
		out[i] = notify.Expiring{Name: e.Name, DaysLeft: e.DaysLeft}
	}
	return out
}

// cmdNotify replaces send_telegram (§4.9): read a Report as JSON on stdin,
// compose the single-line status text, send it. A send failure is
// non-fatal in the reference bash (`|| echo "telegram send failed
// (non-fatal)" >&2`) and stays that way here: the message goes to stderr,
// but this subcommand still exits 0, because nothing about a failed alert
// changes whether the run it describes succeeded.
func cmdNotify(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss notify < report.json")
		return 2
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "notify: reading stdin: %v\n", err)
		return 1
	}
	var dto reportDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		fmt.Fprintf(stderr, "notify: parsing report: %v\n", err)
		return 1
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	dir := secrets.Dir{Root: cfg.SecretsDir}
	tg, err := loadTelegram(dir, getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "notify: %v\n", err)
		return 1
	}

	text := notify.Compose(dto.toReport())
	fmt.Fprintln(stdout, text)
	if err := tg.Send(ctx, text); err != nil {
		fmt.Fprintf(stderr, "telegram send failed (non-fatal): %v\n", err)
	}
	return 0
}
