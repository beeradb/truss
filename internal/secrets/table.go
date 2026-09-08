package secrets

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Expiries is the authored review-date table for hand-made items --
// `credentials/expiries.json` in the platform repo, loaded fresh from the
// checkout on every run. It is never a fallback for a missing probe or a
// substitute for a minted item's real expiry: it exists only for the
// credentials nothing can ask an issuer about (design §5).
//
// A lookup follows the ordinary map idiom: `v, ok := table[item]`. An
// absent item means NO WRITE, never "never" -- that distinction is the
// whole reason this type is a map and not a function with a default.
type Expiries map[string]string

// expiryEntry is the on-disk shape of one table row. Why is carried
// through parsing (so a hand-authored reason survives round-tripping this
// file) but is not itself validated -- the four rules below are the ones
// this project has actually paid for.
type expiryEntry struct {
	Expires string `json:"expires"`
	Why     string `json:"why"`
}

// LoadExpiries reads and validates the authored expiry table from r.
//
// Every entry must be an error, not a warning, per §5:
//
//  1. Every value either parses via DaysUntil (RFC3339 or a bare
//     "2006-01-02" date) or is exactly "never". Nothing else is accepted
//     silently -- an unparseable value here would otherwise reach Vault
//     verbatim and the sweep would silently fail to alarm on it.
//  2. No entry names a probed item. A probed item's expiry comes from its
//     own issuer (internal/secrets.Probe), read live on every sweep; a
//     table entry for it would be dead data that could contradict the
//     issuer and nothing would ever notice, because Sweep.Run skips
//     probed items before it ever looks at what a Store recorded for
//     them.
//  3. Not every entry is "never". An all-"never" table is the exact motion
//     that silences the daily alarm forever, and it must not be
//     expressible by an honest reading of this file -- see the design's
//     "blast radius 2".
//  4. The file is not empty. A table with nothing in it is indistinguishable
//     from a table that was never wired up, and refusing it loudly is
//     cheaper than a caller silently treating "no entries" as "nothing to
//     write" and reporting a clean pass over a broken config.
//
// LoadExpiries never defaults, computes or infers an expiry. "never" can
// only ever arrive because a person typed it into this file; an item
// simply absent from the table is not touched by anything this function
// returns.
func LoadExpiries(r io.Reader, probed []string) (Expiries, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secrets: reading the expiry table: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("secrets: the expiry table is empty -- an empty table is indistinguishable from one that was never wired up, and every hand-made item would silently get no expiry write")
	}

	var raw map[string]expiryEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("secrets: parsing the expiry table: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("secrets: the expiry table lists no items")
	}

	probedSet := make(map[string]bool, len(probed))
	for _, p := range probed {
		probedSet[p] = true
	}

	// Sorted so a multi-problem error message is stable across runs.
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []string
	out := make(Expiries, len(raw))
	allNever := true

	for _, name := range names {
		entry := raw[name]

		if probedSet[name] {
			problems = append(problems, fmt.Sprintf("%q is a probed item -- its expiry comes from its own issuer on every sweep and must not be authored here", name))
			continue
		}

		if entry.Expires != "never" {
			if _, ok := DaysUntil(time.Time{}, entry.Expires); !ok {
				problems = append(problems, fmt.Sprintf("%q has an expires value %q that is neither a valid date nor exactly \"never\"", name, entry.Expires))
				continue
			}
			allNever = false
		}

		out[name] = entry.Expires
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("secrets: the expiry table is invalid: %s", strings.Join(problems, "; "))
	}
	if allNever {
		return nil, fmt.Errorf("secrets: every entry in the expiry table is \"never\" -- that silences the daily expiry alarm for every hand-made item permanently, and must be a deliberate, reviewed exception per item rather than the whole table")
	}

	return out, nil
}
