package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/beeradb/truss/internal/inventory"
)

// cmdInventory dispatches `inventory validate [dir]`, the one verb this
// subcommand has. Like ledger and gate, one top-level word collapses what
// would otherwise be its own subcommand -- there is only one verb here, but
// the same collapse keeps `inventory` itself out of the dispatch table.
//
// It needs no config.Config and no getenv: unlike ledger or gate, nothing it
// does depends on credentials or a forge client, only the tree on disk --
// which is why, like plan-digest, it takes no context either.
func cmdInventory(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: truss inventory validate [dir] [--json]"
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	dir := "."
	dirSet := false
	jsonOut := false
	for _, a := range args[1:] {
		if a == "--json" {
			if jsonOut {
				fmt.Fprintln(stderr, usage)
				return 2
			}
			jsonOut = true
			continue
		}
		if dirSet {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		dir, dirSet = a, true
	}

	// inventory.Load never touches the real filesystem itself (that is what
	// TestInventoryPerformsNoIO enforces); os.DirFS is the one place in this
	// command that does, matching production's own caller.
	snapshot, loadProblems := inventory.Load(os.DirFS(dir))
	checkProblems := inventory.Check(snapshot)

	// Non-nil even when empty: json.Marshal(nil) prints "null", and a
	// programmatic consumer parsing --json output for an array should never
	// have to special-case that "null" means "no problems".
	problems := make([]string, 0, len(loadProblems)+len(checkProblems))
	problems = append(problems, loadProblems...)
	problems = append(problems, checkProblems...)

	if jsonOut {
		out, err := json.Marshal(problems)
		if err != nil {
			// Unreachable: []string always marshals. Kept as a refusal
			// rather than a panic so a future change to problems' type
			// fails loudly here instead of silently in a CI consumer.
			fmt.Fprintf(stderr, "inventory validate: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
	} else if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stdout, p)
		}
	} else {
		// A silent success and "the command did not run at all" print
		// identically in a CI log. Naming the counts is the cheapest way to
		// tell them apart without changing the exit code contract.
		fmt.Fprintf(stdout, "inventory: ok (%d hosts, %d clusters, %d projects, %d environments, %d delivery units)\n",
			len(snapshot.Hosts), len(snapshot.Clusters), len(snapshot.Projects),
			len(snapshot.Environments), len(snapshot.DeliveryUnits))
	}

	if len(problems) > 0 {
		return 1
	}
	return 0
}
