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
		// notify.Report's own doc: "Subject is a field rather than a
		// literal because this repository is the shareable engine;
		// cmd/truss defaults it so the emitted text is unchanged."
		subject = "platform applier"
	}
	return notify.Report{
		Subject:        subject,
		LastSHA:        d.LastSHA,
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
	tg, err := loadTelegram(dir)
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
