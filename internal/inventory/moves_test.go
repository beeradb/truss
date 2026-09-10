package inventory

import (
	"reflect"
	"strings"
	"testing"
)

// k8sEnv and vmEnv build just enough of an Environment for CheckMoves,
// which reads only Shape, Placement and Stateful. Other fields (Schema,
// Project, Vault...) are Check's concern, not this function's, and are left
// zero so each case names only what it is testing.
func k8sEnv(cluster, namespace string, stateful *bool) Environment {
	return Environment{
		Shape:     "kubernetes",
		Placement: Placement{Cluster: strPtr(cluster), Namespace: strPtr(namespace)},
		Stateful:  stateful,
	}
}

func vmEnv(host string, stateful *bool) Environment {
	return Environment{
		Shape:     "vm",
		Placement: Placement{Host: strPtr(host)},
		Stateful:  stateful,
	}
}

func snap(envs map[string]Environment) Snapshot {
	return Snapshot{Environments: envs}
}

func TestCheckMoves(t *testing.T) {
	tests := []struct {
		name      string
		before    Snapshot
		after     Snapshot
		wantEmpty bool
		wantWords []string // all must appear in the joined output
	}{
		{
			name:   "StatefulEnvironmentChangesCluster",
			before: snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))}),
			after:  snap(map[string]Environment{"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(true))}),
			wantWords: []string{
				"inventory/environments/wren/prod.json",
				`"alpha"`, `"beta"`,
				"persistent volumes do not move between clusters",
				"migrated deliberately",
			},
		},
		{
			name:      "SameClusterMoveButStateless",
			before:    snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(false))}),
			after:     snap(map[string]Environment{"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(false))}),
			wantEmpty: true,
		},
		{
			name:   "StatefulEnvironmentChangesHost",
			before: snap(map[string]Environment{"juni/prod": vmEnv("host-a", boolPtr(true))}),
			after:  snap(map[string]Environment{"juni/prod": vmEnv("host-b", boolPtr(true))}),
			wantWords: []string{
				"inventory/environments/juni/prod.json",
				`"host-a"`, `"host-b"`,
				"persistent volumes do not move between hosts",
			},
		},
		{
			name:   "StatefulEnvironmentChangesShape",
			before: snap(map[string]Environment{"juni/prod": vmEnv("host-a", boolPtr(true))}),
			after:  snap(map[string]Environment{"juni/prod": k8sEnv("alpha", "juni-prod", boolPtr(true))}),
			wantWords: []string{
				"inventory/environments/juni/prod.json",
				"re-platform, not a move",
			},
		},
		{
			// The trap: an environment that simply did not exist in
			// `before` must never read as a placement change, or every
			// newly created stateful environment would be refused.
			name:      "StatefulEnvironmentAppearingForTheFirstTime",
			before:    snap(map[string]Environment{}),
			after:     snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))}),
			wantEmpty: true,
		},
		{
			name:      "EnvironmentDisappearing",
			before:    snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))}),
			after:     snap(map[string]Environment{}),
			wantEmpty: true,
		},
		{
			name:      "NamespaceChangeOnTheSameCluster",
			before:    snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))}),
			after:     snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-staging", boolPtr(true))}),
			wantEmpty: true,
		},
		{
			name: "SeveralOffendingEnvironmentsAtOnce",
			before: snap(map[string]Environment{
				"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true)),
				"juni/prod": vmEnv("host-a", boolPtr(true)),
				"argo/prod": k8sEnv("alpha", "argo-prod", boolPtr(true)),
			}),
			after: snap(map[string]Environment{
				"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(true)),
				"juni/prod": vmEnv("host-b", boolPtr(true)),
				"argo/prod": k8sEnv("alpha", "argo-prod", boolPtr(true)), // unchanged
			}),
			wantWords: []string{
				"inventory/environments/wren/prod.json",
				"inventory/environments/juni/prod.json",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckMoves(tt.before, tt.after)
			if tt.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("CheckMoves = %v, want none", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("CheckMoves returned nothing, want a refusal")
			}
			joined := strings.Join(got, "\n")
			for _, w := range tt.wantWords {
				if !strings.Contains(joined, w) {
					t.Errorf("no problem contained %q; got: %v", w, got)
				}
			}
		})
	}
}

// TestSeveralOffendingEnvironmentsAreSortedAndBothReported pins the count
// and the order separately from the substring checks above: exactly two
// problems (not the third, unchanged environment), and sorted.
func TestSeveralOffendingEnvironmentsAreSortedAndBothReported(t *testing.T) {
	before := snap(map[string]Environment{
		"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true)),
		"juni/prod": vmEnv("host-a", boolPtr(true)),
		"argo/prod": k8sEnv("alpha", "argo-prod", boolPtr(true)),
	})
	after := snap(map[string]Environment{
		"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(true)),
		"juni/prod": vmEnv("host-b", boolPtr(true)),
		"argo/prod": k8sEnv("alpha", "argo-prod", boolPtr(true)),
	})

	got := CheckMoves(before, after)
	if len(got) != 2 {
		t.Fatalf("CheckMoves = %v, want exactly 2 problems", got)
	}
	if !strings.Contains(got[0], "juni/prod") || !strings.Contains(got[1], "wren/prod") {
		t.Errorf("CheckMoves = %v, want juni/prod before wren/prod (sorted)", got)
	}
}

// TestCheckMovesIsPure calls CheckMoves twice on the same pair of
// snapshots and requires the same answer both times, and requires neither
// snapshot to come out mutated.
func TestCheckMovesIsPure(t *testing.T) {
	before := snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))})
	after := snap(map[string]Environment{"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(true))})
	beforeCopy := snap(map[string]Environment{"wren/prod": k8sEnv("alpha", "wren-prod", boolPtr(true))})
	afterCopy := snap(map[string]Environment{"wren/prod": k8sEnv("beta", "wren-prod", boolPtr(true))})

	got1 := CheckMoves(before, after)
	got2 := CheckMoves(before, after)
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("CheckMoves called twice gave different answers:\n%v\n%v", got1, got2)
	}
	if !reflect.DeepEqual(before, beforeCopy) {
		t.Fatalf("CheckMoves mutated its before snapshot")
	}
	if !reflect.DeepEqual(after, afterCopy) {
		t.Fatalf("CheckMoves mutated its after snapshot")
	}
}
