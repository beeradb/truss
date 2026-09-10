package gates

import "testing"

// --- CheckAnsibleTargets ------------------------------------------------

// TestCheckAnsibleTargetsPassesACleanTargetSet: declared hosts, nothing
// unknown, nothing unreachable -- the ordinary case -- clears the gate.
func TestCheckAnsibleTargetsPassesACleanTargetSet(t *testing.T) {
	targets := AnsibleTargets{
		Play:     "ansible/plays/dev-beta",
		Declared: []string{"dev-beta.invalid"},
	}
	if problems := CheckAnsibleTargets(targets); len(problems) != 0 {
		t.Fatalf("a clean target set was refused: %v", problems)
	}
}

// TestCheckAnsibleTargetsRefusesAnyUnknown: a single unknown-tagged device
// is refused, and the refusal says both what it is (a machine claiming to
// be managed that nobody declared) and that it stops the whole pass, not
// only this play.
func TestCheckAnsibleTargetsRefusesAnyUnknown(t *testing.T) {
	targets := AnsibleTargets{
		Play:     "ansible/plays/dev-beta",
		Declared: []string{"dev-beta.invalid"},
		Unknown:  []string{"intruder.invalid"},
	}
	problems := CheckAnsibleTargets(targets)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "intruder.invalid") {
		t.Errorf("refusal does not name the unknown device: %v", problems)
	}
	if !hasProblemContaining(problems, "every play in this pass") {
		t.Errorf("refusal does not say it stops every play in the pass: %v", problems)
	}
}

// TestCheckAnsibleTargetsNamesSeveralUnknowns: every unknown device is
// named, not just the first -- an operator investigating an intruder must
// not have to go looking for the rest.
func TestCheckAnsibleTargetsNamesSeveralUnknowns(t *testing.T) {
	targets := AnsibleTargets{
		Play:     "ansible/plays/dev-beta",
		Declared: []string{"dev-beta.invalid"},
		Unknown:  []string{"intruder-one.invalid", "intruder-two.invalid", "intruder-three.invalid"},
	}
	problems := CheckAnsibleTargets(targets)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem (Unknown is one fact about the play), got %v", problems)
	}
	for _, want := range []string{"intruder-one.invalid", "intruder-two.invalid", "intruder-three.invalid"} {
		if !hasProblemContaining(problems, want) {
			t.Errorf("no problem named %s: %v", want, problems)
		}
	}
}

// TestCheckAnsibleTargetsRefusesAnyUnreachable: a declared host absent from
// the tailnet is refused, never silently skipped -- absent is not "fine".
func TestCheckAnsibleTargetsRefusesAnyUnreachable(t *testing.T) {
	targets := AnsibleTargets{
		Play:        "ansible/plays/dev-beta",
		Declared:    []string{"dev-beta.invalid"},
		Unreachable: []string{"dev-beta.invalid"},
	}
	problems := CheckAnsibleTargets(targets)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "dev-beta.invalid") {
		t.Errorf("refusal does not name the unreachable host: %v", problems)
	}
}

// TestCheckAnsibleTargetsRefusesAnEmptyDeclared: a play targeting nothing is
// refused outright, because running it with no limit built from an empty
// set would target every host in the inventory.
func TestCheckAnsibleTargetsRefusesAnEmptyDeclared(t *testing.T) {
	problems := CheckAnsibleTargets(AnsibleTargets{Play: "ansible/plays/dev-beta"})
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "no declared hosts") {
		t.Errorf("refusal does not say the target set is empty: %v", problems)
	}
}

// TestCheckAnsibleTargetsReportsEveryOffender: several independent problems
// on the same play are all reported, matching CheckDeclarations's own
// convention -- an operator fixing refusals one at a time, re-running the
// whole pass between each, is the outcome this function exists to avoid.
func TestCheckAnsibleTargetsReportsEveryOffender(t *testing.T) {
	targets := AnsibleTargets{
		Play:        "ansible/plays/dev-beta",
		Unknown:     []string{"intruder.invalid"},
		Unreachable: []string{"dev-beta.invalid"},
	}
	problems := CheckAnsibleTargets(targets)
	if len(problems) != 3 {
		t.Fatalf("want exactly three problems (unknown, unreachable, empty declared), got %d: %v", len(problems), problems)
	}
}
