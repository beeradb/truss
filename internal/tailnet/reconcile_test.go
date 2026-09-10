package tailnet

import (
	"reflect"
	"testing"
	"time"
)

const testManagedTag = "tag:k8s"

// now is fixed once for every case below and staleness is always expressed
// as an offset from it -- never a hard-coded date. internal/parity/corpus.go
// records why: a corpus built from instants rots into meaning something
// different a year later.
var now = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

const staleAfter = 24 * time.Hour

func TestReconcile(t *testing.T) {
	cases := []struct {
		name              string
		devices           []Device
		declared          []string
		wantUnknownTagged []string
		wantUnreachable   []string
	}{
		{
			name:              "zero value: no devices, no declared hosts",
			devices:           nil,
			declared:          nil,
			wantUnknownTagged: nil,
			wantUnreachable:   nil,
		},
		{
			name: "a tagged device with no declared record is UnknownTagged",
			devices: []Device{
				{Name: "ghost", Tags: []string{testManagedTag}, LastSeen: now},
			},
			declared:          nil,
			wantUnknownTagged: []string{"ghost"},
			wantUnreachable:   nil,
		},
		{
			name:              "a declared host with no matching device is Unreachable",
			devices:           nil,
			declared:          []string{"web-1"},
			wantUnknownTagged: nil,
			wantUnreachable:   []string{"web-1"},
		},
		{
			name: "a declared host last seen beyond staleAfter is Unreachable",
			devices: []Device{
				{Name: "web-1", Tags: []string{testManagedTag}, LastSeen: now.Add(-48 * time.Hour)},
			},
			declared:          []string{"web-1"},
			wantUnknownTagged: nil,
			wantUnreachable:   []string{"web-1"},
		},
		{
			name: "a declared host seen recently is neither",
			devices: []Device{
				{Name: "web-1", Tags: []string{testManagedTag}, LastSeen: now.Add(-1 * time.Hour)},
			},
			declared:          []string{"web-1"},
			wantUnknownTagged: nil,
			wantUnreachable:   nil,
		},
		{
			name: "an untagged unknown device is neither",
			devices: []Device{
				{Name: "laptop", Tags: nil, LastSeen: now},
			},
			declared:          nil,
			wantUnknownTagged: nil,
			wantUnreachable:   nil,
		},
		{
			name: "several of each at once, all reported and sorted",
			devices: []Device{
				{Name: "web-2", Tags: []string{testManagedTag}, LastSeen: now.Add(-1 * time.Hour)},  // declared, recent -> neither
				{Name: "web-3", Tags: []string{testManagedTag}, LastSeen: now.Add(-72 * time.Hour)}, // declared, stale -> Unreachable
				{Name: "ghost-b", Tags: []string{testManagedTag}, LastSeen: now},                    // undeclared, tagged -> UnknownTagged
				{Name: "ghost-a", Tags: []string{testManagedTag}, LastSeen: now},                    // undeclared, tagged -> UnknownTagged
				{Name: "laptop", Tags: nil, LastSeen: now},                                          // undeclared, untagged -> neither
			},
			declared:          []string{"web-1", "web-2", "web-3"},
			wantUnknownTagged: []string{"ghost-a", "ghost-b"},
			wantUnreachable:   []string{"web-1", "web-3"},
		},
		{
			name: "duplicate declared hosts and duplicate devices are de-duplicated",
			devices: []Device{
				{Name: "ghost", Tags: []string{testManagedTag}, LastSeen: now},
				{Name: "ghost", Tags: []string{testManagedTag}, LastSeen: now},
			},
			declared:          []string{"web-1", "web-1"},
			wantUnknownTagged: []string{"ghost"},
			wantUnreachable:   []string{"web-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Reconcile(tc.devices, tc.declared, testManagedTag, now, staleAfter)
			if !reflect.DeepEqual(got.UnknownTagged, tc.wantUnknownTagged) {
				t.Errorf("UnknownTagged = %v, want %v", got.UnknownTagged, tc.wantUnknownTagged)
			}
			if !reflect.DeepEqual(got.Unreachable, tc.wantUnreachable) {
				t.Errorf("Unreachable = %v, want %v", got.Unreachable, tc.wantUnreachable)
			}
		})
	}
}
