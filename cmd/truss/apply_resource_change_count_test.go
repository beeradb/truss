package main

import "testing"

// countResourceChanges is a DELIBERATE DIVERGENCE from the bash's
// summary_from_plan (apply.sh:598), which counts every entry in
// resource_changes, no-ops included. Measured in production: a
// credentials/ plan with 30 no-op resources and zero real changes was
// reported as "rotated credentials (30 changes)", and would say so every
// night. See internal/parity/divergences.go's COUNT-EXCLUDES-NOOP for the
// full argument and cmd/truss/apply_cmd.go's countResourceChanges for the
// real `tofu show -json` evidence behind the actions shapes used below.
func TestCountResourceChangesExcludesNoOps(t *testing.T) {
	cases := []struct {
		name      string
		planJSON  string
		wantCount int
		wantOK    bool
	}{
		{
			// The load-bearing case: this is the production shape tonight.
			// A plan whose every resource is a no-op must count as zero
			// changes, not as the number of resources the plan looked at.
			name: "a plan of all no-op resources counts zero changes",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"]}},
				{"address":"b","change":{"actions":["no-op"]}},
				{"address":"c","change":{"actions":["no-op"]}}
			]}`,
			wantCount: 0,
			wantOK:    true,
		},
		{
			name: "a mix of no-op and update counts only the update",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"]}},
				{"address":"b","change":{"actions":["update"]}},
				{"address":"c","change":{"actions":["no-op"]}}
			]}`,
			wantCount: 1,
			wantOK:    true,
		},
		{
			// Verified against a real `tofu show -json`: a forced replace
			// is reported as the two-element ["delete","create"] on ONE
			// resource_changes entry, not two separate entries. It must
			// count as one changed resource.
			name: "a delete-create replace counts as one changed resource, not two",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["delete","create"]}}
			]}`,
			wantCount: 1,
			wantOK:    true,
		},
		{
			name:      "an empty resource_changes array counts zero",
			planJSON:  `{"resource_changes":[]}`,
			wantCount: 0,
			wantOK:    true,
		},
		{
			name:      "malformed plan JSON returns the existing could-not-parse false, unchanged",
			planJSON:  `{"resource_changes": not valid json`,
			wantCount: 0,
			wantOK:    false,
		},
		{
			// Fail loud rather than silently swallowing a shape OpenTofu
			// is not known to emit: an entry with no actions at all (or an
			// empty actions array) is counted as a change rather than
			// treated as a no-op by omission.
			name: "an entry with a missing actions array is counted, not skipped",
			planJSON: `{"resource_changes":[
				{"address":"a"}
			]}`,
			wantCount: 1,
			wantOK:    true,
		},
		{
			name: "an entry with an empty actions array is counted, not skipped",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":[]}}
			]}`,
			wantCount: 1,
			wantOK:    true,
		},
		{
			// ⚠️ THE CALLER SKIPS THE DIGEST GATE ON (0, true), so a
			// document with no resource_changes key at all must NOT report
			// it. `{}` unmarshals happily, and reading that as "nothing to
			// change" turned every fail-closed refusal into a silent apply
			// for any ShowJSON that came back JSON-shaped but not a plan.
			name:      "a document with no resource_changes key is unreadable, not empty",
			planJSON:  `{}`,
			wantCount: 0,
			wantOK:    false,
		},
		{
			name:      "a null resource_changes is unreadable, not empty",
			planJSON:  `{"resource_changes":null}`,
			wantCount: 0,
			wantOK:    false,
		},
		{
			// An import block whose resource already matches configuration
			// is rendered as a no-op with `importing` set, and applying it
			// still writes the resource into state. It is a change, so it
			// is counted -- which is what keeps the plan gated.
			name: "a no-op carrying an import is counted, not skipped",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"],"importing":{"id":"abc"}}}
			]}`,
			wantCount: 1,
			wantOK:    true,
		},
		{
			name: "a no-op with a null importing is still a no-op",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"],"importing":null}}
			]}`,
			wantCount: 0,
			wantOK:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, ok := countResourceChanges([]byte(tc.planJSON))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && count != tc.wantCount {
				t.Fatalf("count = %d, want %d", count, tc.wantCount)
			}
		})
	}
}
