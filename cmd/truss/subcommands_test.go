package main

import (
	"reflect"
	"testing"
)

// TestSubcommandsAreExactlyTheDocumentedSet pins the dispatch table to
// docs/port-plan.md §4.9's table exactly: ledger, plan-digest, token, gate,
// expiry, notify, apply -- no more, no fewer. "ledger get"/"ledger put" and
// "gate protection"/"gate commit" collapse to one top-level verb each,
// which is why this is seven entries against the doc's eight rows.
func TestSubcommandsAreExactlyTheDocumentedSet(t *testing.T) {
	want := []string{
		"ledger",
		"plan-digest",
		"token",
		"gate",
		"expiry",
		"notify",
		"apply",
	}
	if !reflect.DeepEqual(subcommands, want) {
		t.Fatalf("subcommands = %v, want %v", subcommands, want)
	}

	// Every documented subcommand must actually dispatch -- isSubcommand is
	// the guard runEnv uses to decide between "usage" and "route it", so a
	// name present in the slice but absent from the switch in runEnv would
	// otherwise fail silently at the "unreachable" default case.
	for _, name := range want {
		if !isSubcommand(name) {
			t.Errorf("isSubcommand(%q) = false, want true", name)
		}
	}
	if isSubcommand("bogus") {
		t.Errorf("isSubcommand(%q) = true, want false", "bogus")
	}
}
