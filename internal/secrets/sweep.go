package secrets

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Expiring is one line of the sweep's report: a name and, when known, the
// number of days left. DaysLeft is nil for an item with no `expires` at
// all or one that could not be parsed -- reported, never skipped, because
// a gap is a credential that will expire unannounced.
type Expiring struct {
	Name     string `json:"name"`
	DaysLeft *int   `json:"days_left"`
}

// Sweep is one daily pass over every configured Store and Probe.
type Sweep struct {
	// Stores are swept in order; the same item title in two mounts is
	// reported twice (the bash does not dedupe).
	Stores []Store
	// Probes maps an item name to the issuer that answers for it directly.
	// A probed item's name is never read out of a Store's metadata (§4.7:
	// "Probed items never have their expires read").
	Probes map[string]Probe
	// WarnDays is the inclusive boundary: an item with exactly this many
	// days left is reported, one more is not.
	WarnDays int
	// Now returns the current time. Defaults to time.Now; tests override
	// it for a reproducible "days left".
	Now func() time.Time
}

// Run sweeps every probe, then every store, and returns everything within
// WarnDays plus everything with no recorded (or no parseable) expiry.
//
// Run returns its error; it never terminates the process and it never
// discards findings gathered before the error occurred (§4.7, §2.16): a
// caller that gets (findings, err) with err non-nil must still treat
// findings as real and still deliver both together, because this is the
// last step of the daily pass and an earlier drift result must not be lost
// by this one failing quietly.
//
// Three things are errors rather than results, matched here:
//   - a Store.List or Store.Expiry call that fails outright (a login,
//     transport, or read failure) stops the sweep on that store and
//     returns everything gathered so far alongside the error;
//   - a store that lists items but records `expires` on not one of them
//     -- indistinguishable, item by item, from a store of legitimate
//     gaps, and the prerequisite's own alarm; one recorded expiry, even
//     "never", disarms it;
//   - (implicitly) never treating a Probe or Store failure as a synonym
//     for "nothing is expiring": a probe failure reports as an unrecorded
//     expiry (a finding), a store failure reports as a returned error.
func (s Sweep) Run(ctx context.Context) ([]Expiring, error) {
	now := s.Now
	if now == nil {
		now = time.Now
	}

	var findings []Expiring

	probedNames := make([]string, 0, len(s.Probes))
	for name := range s.Probes {
		probedNames = append(probedNames, name)
	}
	sort.Strings(probedNames)
	probed := make(map[string]bool, len(probedNames))

	for _, name := range probedNames {
		probed[name] = true
		t, ok, err := s.Probes[name].Expiry(ctx)
		if err != nil || !ok {
			findings = append(findings, Expiring{Name: name})
			continue
		}
		days := daysBetween(now(), t)
		if days <= s.WarnDays {
			d := days
			findings = append(findings, Expiring{Name: name, DaysLeft: &d})
		}
	}

	for _, store := range s.Stores {
		items, err := store.List(ctx)
		if err != nil {
			return findings, fmt.Errorf("secrets: could not list %s: %w", store.Name(), err)
		}

		var mountFindings []Expiring
		recorded := false
		eligible := 0

		for _, item := range items {
			if probed[item] {
				continue
			}
			eligible++

			raw, itemRecorded, err := store.Expiry(ctx, item)
			if err != nil {
				return findings, fmt.Errorf("secrets: could not read the expiry of %q in %s: %w", item, store.Name(), err)
			}
			if itemRecorded {
				recorded = true
			}
			if raw == "never" {
				continue
			}

			days, ok := DaysUntil(now(), raw)
			if !ok {
				mountFindings = append(mountFindings, Expiring{Name: item})
				continue
			}
			if days <= s.WarnDays {
				d := days
				mountFindings = append(mountFindings, Expiring{Name: item, DaysLeft: &d})
			}
		}

		if eligible > 0 && !recorded {
			return findings, fmt.Errorf("secrets: %s lists %d item(s) but not one records an expiry -- has the write side that seeds Vault's custom_metadata been wired up? (docs/port-plan.md §4.7, \"BLOCKED: nothing writes expires into Vault\")", store.Name(), eligible)
		}

		findings = append(findings, mountFindings...)
	}

	return findings, nil
}

// daysBetween is DaysUntil for a Probe's already-parsed time.Time.
func daysBetween(now, target time.Time) int {
	return int((target.Unix() - now.Unix()) / 86400)
}

// DaysUntil parses raw as an RFC3339 instant or a bare "2006-01-02" date
// and returns the whole number of days from now to it, truncated toward
// zero to match the bash's `$(( (target - now) / 86400 ))` (bash and Go
// integer division both truncate toward zero, so a date twelve hours past
// reads 0, not -1). ok is false when raw parses as neither shape.
func DaysUntil(now time.Time, raw string) (days int, ok bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return daysBetween(now, t), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return daysBetween(now, t), true
	}
	return 0, false
}
