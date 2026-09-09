package main

import (
	"strings"
	"testing"
)

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
		wantErr   string // a substring of the refusal, empty when readable
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
			wantErr:   "",
		},
		{
			name: "a mix of no-op and update counts only the update",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"]}},
				{"address":"b","change":{"actions":["update"]}},
				{"address":"c","change":{"actions":["no-op"]}}
			]}`,
			wantCount: 1,
			wantErr:   "",
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
			wantErr:   "",
		},
		{
			name:      "an empty resource_changes array counts zero",
			planJSON:  `{"resource_changes":[]}`,
			wantCount: 0,
			wantErr:   "",
		},
		{
			name:      "malformed plan JSON is unreadable, and says so",
			planJSON:  `{"resource_changes": not valid json`,
			wantCount: 0,
			wantErr:   "does not parse",
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
			wantErr:   "",
		},
		{
			name: "an entry with an empty actions array is counted, not skipped",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":[]}}
			]}`,
			wantCount: 1,
			wantErr:   "",
		},
		{
			// ⚠️ THE CALLER SKIPS THE DIGEST GATE ON (0, true), so a
			// document with no resource_changes key at all must NOT report
			// it. `{}` unmarshals happily, and reading that as "nothing to
			// change" turned every fail-closed refusal into a silent apply
			// for any ShowJSON that came back JSON-shaped but not a plan.
			// ⚠️ MEASURED, NOT ASSUMED. OpenTofu marshals resource_changes
			// omitempty and errored NOT, so a plan that changes nothing
			// omits the first and always carries the second. `{}` carries
			// neither and is not a plan at all.
			name:      "a document carrying neither key is not a plan",
			planJSON:  `{}`,
			wantCount: 0,
			wantErr:   "not a plan document",
		},
		{
			name:      "a plan whose resource_changes was omitted counts zero, and is not refused",
			planJSON:  `{"format_version":"1.2","errored":false}`,
			wantCount: 0,
			wantErr:   "",
		},
		{
			// The same hole one layer in: the key is there and carries
			// nothing, so it is no evidence that this is a plan.
			name:      "a null resource_changes with no errored is not a plan",
			planJSON:  `{"resource_changes":null}`,
			wantCount: 0,
			wantErr:   "not a plan document",
		},
		{
			// errored is a plain bool in OpenTofu's own struct, so a
			// document carrying the name and not the fact is not one.
			name:      "a null errored is not a plan either",
			planJSON:  `{"errored":null}`,
			wantCount: 0,
			wantErr:   "not a plan document",
		},
		{
			name:      "an errored that is not a boolean is not a plan",
			planJSON:  `{"errored":"nope"}`,
			wantCount: 0,
			wantErr:   "not a plan document",
		},
		{
			// errored:true with the changes key omitted otherwise reads as a
			// valid plan that changes nothing, which skips the digest gate.
			// It is the one value of that field meaning the plan is not
			// applyable.
			name:      "an errored plan is refused, not read as zero changes",
			planJSON:  `{"errored":true}`,
			wantCount: 0,
			wantErr:   "reports errored",
		},
		{
			name:      "an errored plan with changes in it is refused too",
			planJSON:  `{"errored":true,"resource_changes":[{"address":"a","change":{"actions":["update"]}}]}`,
			wantCount: 0,
			wantErr:   "reports errored",
		},
		{
			name:      "a null resource_changes on a real plan counts zero",
			planJSON:  `{"errored":false,"resource_changes":null}`,
			wantCount: 0,
			wantErr:   "",
		},
		{
			name:      "a resource_changes that is not a list is refused by name",
			planJSON:  `{"errored":false,"resource_changes":"nope"}`,
			wantCount: 0,
			wantErr:   "does not read as a list",
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
			wantErr:   "",
		},
		{
			name: "a no-op with a null importing is still a no-op",
			planJSON: `{"resource_changes":[
				{"address":"a","change":{"actions":["no-op"],"importing":null}}
			]}`,
			wantCount: 0,
			wantErr:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, err := countResourceChanges([]byte(tc.planJSON))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want it readable", err)
				}
				if count != tc.wantCount {
					t.Fatalf("count = %d, want %d", count, tc.wantCount)
				}
				return
			}
			if err == nil {
				t.Fatalf("read as %d changes, want a refusal naming %q", count, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
