package inventory

import (
	"fmt"
	"sort"
)

// CheckMoves compares two snapshots of the same inventory -- before is what
// it was, after is what it is now -- and refuses a move that would silently
// lose data. Check alone cannot see this: it takes one Snapshot, so a
// workload's placement changing between two commits is invisible to it --
// the second Snapshot on its own just shows the new placement, with nothing
// recording that anything changed. This is the one function in the package
// that is handed two.
//
// Moving a stateless workload between clusters is a small, reviewable diff:
// the delivery unit moves, Flux creates on one side and prunes on the other.
// Moving a stateful one is a data migration -- PVCs do not follow a
// placement change, and nothing in this system makes them -- so the one
// thing CheckMoves must never do is let that happen quietly. It cannot fix
// the migration; it can only stop the move from passing as if it were the
// stateless kind.
func CheckMoves(before, after Snapshot) []string {
	var problems []string

	keys := make([]string, 0, len(after.Environments))
	for key := range after.Environments {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		a := after.Environments[key]

		// ⚠️ An environment absent from `before` has not moved -- it was
		// created. Treating "not in the old snapshot" as a placement change
		// would refuse every newly created stateful environment, which is
		// the ordinary case, not the one this function exists to catch.
		b, existed := before.Environments[key]
		if !existed {
			continue
		}

		// An environment that has never said whether it holds state is
		// refused elsewhere, by Check's nil check on Stateful -- not read
		// here as either answer. Only an explicit true is grounds for a
		// refusal; nil and false both pass CheckMoves silently.
		if a.Stateful == nil || !*a.Stateful {
			continue
		}

		path := environmentPath(key)

		if b.Shape != a.Shape {
			problems = append(problems, fmt.Sprintf(
				"%s: is stateful and its shape changed from %q to %q -- that is a re-platform, not a move; migrate the data deliberately and split it into its own change",
				path, b.Shape, a.Shape))
			continue
		}

		switch a.Shape {
		case "kubernetes":
			if before := strOrNone(b.Placement.Cluster); before != strOrNone(a.Placement.Cluster) {
				problems = append(problems, fmt.Sprintf(
					"%s: is stateful and placement.cluster changed from %q to %q -- persistent volumes do not move between clusters, so the data must be migrated deliberately and the move split into a separate change",
					path, before, strOrNone(a.Placement.Cluster)))
			}
		case "vm":
			if before := strOrNone(b.Placement.Host); before != strOrNone(a.Placement.Host) {
				problems = append(problems, fmt.Sprintf(
					"%s: is stateful and placement.host changed from %q to %q -- persistent volumes do not move between hosts, so the data must be migrated deliberately and the move split into a separate change",
					path, before, strOrNone(a.Placement.Host)))
			}
		}
		// A namespace change on the same cluster is a rename inside one
		// cluster, not a move across a storage boundary -- disruptive,
		// maybe, but it does not cross the line this function exists to
		// guard. Deliberately not checked here; do not "fix" this by
		// comparing Placement.Namespace too.
	}

	sort.Strings(problems)
	return problems
}

// strOrNone reads a *string placement field as a plain string for
// comparison and for the refusal message, treating nil the same as an
// explicit "none" -- a placement missing its cluster or host is already a
// separate refusal from checkEnvironment, and CheckMoves only needs a value
// it can compare and print, not a fresh opinion about validity.
func strOrNone(s *string) string {
	if s == nil {
		return "(none)"
	}
	return *s
}
