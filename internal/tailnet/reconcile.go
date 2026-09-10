package tailnet

import (
	"sort"
	"time"
)

// Findings is what comparing the tailnet against the committed inventory
// found. Both fields are names, never actions -- Reconcile is a report, and
// nothing it returns is ever acted on automatically (see the package doc).
type Findings struct {
	// UnknownTagged is every device carrying the managed tag that no
	// declared host name matches. This is the dangerous direction: a
	// machine claiming to be managed that nobody declared is either an
	// intruder or a host somebody forgot to record, and both need a
	// person -- git is the source of truth for what is managed, so a tag
	// alone can never add a host to what the applier will touch.
	UnknownTagged []string
	// Unreachable is every declared host with no matching device on the
	// tailnet, or whose device has not been seen within staleAfter of
	// now. ⚠️ Absent is not the same as "fine": a host that dropped off
	// must be named, not silently skipped.
	Unreachable []string
}

// Reconcile compares devices against declared -- the plain names of every
// host git says is managed -- and returns what disagrees. It performs no
// I/O and knows nothing about internal/inventory: the caller maps host
// records to names, which is what keeps this function testable without
// building an inventory fixture, and stops a change to the inventory
// schema from rippling in here.
//
// A device that does not carry managedTag is not this package's business
// and appears in neither list -- there is deliberately no third list for
// it, so that omission is not later "fixed" into a finding.
//
// Nothing here is ever reconciled or removed. This reports.
func Reconcile(devices []Device, declared []string, managedTag string, now time.Time, staleAfter time.Duration) Findings {
	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	// reachable holds every declared name that some device answered to
	// within staleAfter of now. Reachability is about whether the named
	// machine is present on the tailnet at all, not about whether it
	// carries the managed tag -- a tag that fell off a still-live host is
	// a compliance question, not a connectivity one, and this field
	// answers only the latter.
	reachable := map[string]bool{}
	// unknownTagged holds every managed-tagged device whose name is not
	// declared -- the dangerous direction described on the field itself.
	unknownTagged := map[string]bool{}

	for _, d := range devices {
		if now.Sub(d.LastSeen) <= staleAfter {
			reachable[d.Name] = true
		}

		tagged := false
		for _, t := range d.Tags {
			if t == managedTag {
				tagged = true
				break
			}
		}
		if tagged && !declaredSet[d.Name] {
			unknownTagged[d.Name] = true
		}
	}

	unreachable := map[string]bool{}
	for name := range declaredSet {
		if !reachable[name] {
			unreachable[name] = true
		}
	}

	return Findings{
		UnknownTagged: sortedKeys(unknownTagged),
		Unreachable:   sortedKeys(unreachable),
	}
}

// sortedKeys returns m's keys, sorted, de-duplicated by construction because
// m is a set -- so a daily alert built from this output does not churn
// between two runs that found the same facts in a different order. An empty
// set returns nil rather than an empty, non-nil slice, matching this
// repository's convention (e.g. internal/secrets.Store.List) that "found
// nothing" and "found zero of something" are the same value.
func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
