package inventory

import (
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// validSnapshot builds a fully consistent inventory: two hosts, one
// cluster, two projects each with one environment -- one kubernetes shaped
// (with a matching delivery unit), one vm shaped (placed directly on a
// host). Every defect test below starts here and breaks exactly one thing,
// so a problem appearing where TestAValidSnapshotHasNoProblems finds none
// can only be the mutation the test made.
//
// It is a function, not a package variable, so every test gets its own
// maps and slices: mutating the result of one call can never leak into
// another call's Snapshot.
func validSnapshot() Snapshot {
	return Snapshot{
		Hosts: map[string]Host{
			"alpha": {
				Schema:        hostSchema,
				Name:          "alpha",
				Kind:          "vm",
				Role:          "k8s-node",
				ProvisionedBy: "hosts/alpha",
				Config:        strPtr("ansible/alpha"),
				TailnetTags:   []string{"k8s"},
				Cluster:       strPtr("prod"),
			},
			"beta": {
				Schema:        hostSchema,
				Name:          "beta",
				Kind:          "vm",
				Role:          "unmanaged",
				ProvisionedBy: "hosts/beta",
				Config:        nil,
				Cluster:       nil,
			},
		},
		Clusters: map[string]Cluster{
			"prod": {
				Schema:         clusterSchema,
				Name:           "prod",
				Distribution:   "k3s",
				Hosts:          []string{"alpha"},
				Baseline:       "baselines/prod",
				Capabilities:   []string{"ingress", "storage"},
				KubeconfigItem: "prod-kubeconfig",
				HasHAVault:     boolPtr(true),
			},
		},
		Projects: map[string]Project{
			"wren": {Schema: projectSchema, Name: "wren", Environments: []string{"prod"}},
			"juni": {Schema: projectSchema, Name: "juni", Environments: []string{"prod"}},
		},
		Environments: map[string]Environment{
			"wren/prod": {
				Schema:    environmentSchema,
				Project:   "wren",
				Name:      "prod",
				Shape:     "kubernetes",
				Placement: Placement{Cluster: strPtr("prod"), Namespace: strPtr("wren-prod")},
				Requires:  []string{"ingress"},
				Vault:     VaultRef{Mount: "secret", Prefix: "wren/prod"},
				Stateful:  boolPtr(false),
			},
			"juni/prod": {
				Schema:    environmentSchema,
				Project:   "juni",
				Name:      "prod",
				Shape:     "vm",
				Placement: Placement{Host: strPtr("beta")},
				Vault:     VaultRef{Mount: "secret", Prefix: "juni/prod"},
				Stateful:  boolPtr(false),
			},
		},
		DeliveryUnits: []string{"prod/wren-prod"},
	}
}

func TestAValidSnapshotHasNoProblems(t *testing.T) {
	// Without this test, every defect test below passes vacuously against
	// a Check that refuses everything -- there would be nothing proving
	// the fixture itself is actually clean.
	if got := Check(validSnapshot()); len(got) != 0 {
		t.Fatalf("a consistent snapshot was refused: %v", got)
	}
}

// --- one case per refusal --------------------------------------------------

type defect struct {
	name      string
	mutate    func(*Snapshot)
	wantFile  string   // substring the one new problem's file/path reference must contain
	wantWords []string // substrings that, together, must all appear in the one new problem
}

func defects() []defect {
	return []defect{
		{
			name: "UnknownSchema",
			mutate: func(s *Snapshot) {
				h := s.Hosts["alpha"]
				h.Schema = "truss.host/v2"
				s.Hosts["alpha"] = h
			},
			wantFile:  "inventory/hosts/alpha.json",
			wantWords: []string{`"truss.host/v2"`, "not recognised", `"truss.host/v1"`},
		},
		{
			name: "MissingSchema",
			mutate: func(s *Snapshot) {
				h := s.Hosts["alpha"]
				h.Schema = ""
				s.Hosts["alpha"] = h
			},
			wantFile:  "inventory/hosts/alpha.json",
			wantWords: []string{"not recognised"},
		},
		{
			name: "NameDoesNotMatchFilenameStem",
			mutate: func(s *Snapshot) {
				h := s.Hosts["alpha"]
				h.Name = "alph4"
				s.Hosts["alpha"] = h
			},
			wantFile:  "inventory/hosts/alpha.json",
			wantWords: []string{`"alph4"`, `"alpha"`, "rename"},
		},
		// ⚠️ checkCluster, checkProject AND checkEnvironment EACH REPEAT THE
		// SAME SCHEMA-AND-NAME SHAPE checkHost ALREADY HAS ABOVE, AND ONLY THE
		// HOST COPY WAS EVER MUTATION-TESTED. Disabling the schema or name
		// check inside any of the other three left this whole suite green --
		// found by the 2026-09-10 mutation audit. One case per record type,
		// same as Host already gets two.
		{
			name: "ClusterUnknownSchema",
			mutate: func(s *Snapshot) {
				c := s.Clusters["prod"]
				c.Schema = "truss.cluster/v2"
				s.Clusters["prod"] = c
			},
			wantFile:  "inventory/clusters/prod.json",
			wantWords: []string{`"truss.cluster/v2"`, "not recognised", `"truss.cluster/v1"`},
		},
		{
			name: "ClusterNameDoesNotMatchFilenameStem",
			mutate: func(s *Snapshot) {
				c := s.Clusters["prod"]
				c.Name = "prod2"
				s.Clusters["prod"] = c
			},
			wantFile:  "inventory/clusters/prod.json",
			wantWords: []string{`"prod2"`, `"prod"`, "rename"},
		},
		{
			name: "ProjectUnknownSchema",
			mutate: func(s *Snapshot) {
				p := s.Projects["wren"]
				p.Schema = "truss.project/v2"
				s.Projects["wren"] = p
			},
			wantFile:  "inventory/projects/wren.json",
			wantWords: []string{`"truss.project/v2"`, "not recognised", `"truss.project/v1"`},
		},
		{
			name: "ProjectNameDoesNotMatchFilenameStem",
			mutate: func(s *Snapshot) {
				p := s.Projects["wren"]
				// checkProject builds each environment's expected key from
				// p.Name rather than the file's stem, so changing Name alone
				// also breaks that lookup ("wren2/prod" has no record) --
				// a second, real but unrelated refusal. Emptying Environments
				// isolates this case to the name-vs-stem defect alone.
				p.Environments = nil
				p.Name = "wren2"
				s.Projects["wren"] = p
			},
			wantFile:  "inventory/projects/wren.json",
			wantWords: []string{`"wren2"`, `"wren"`, "rename"},
		},
		{
			name: "EnvironmentUnknownSchema",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Schema = "truss.environment/v2"
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"truss.environment/v2"`, "not recognised", `"truss.environment/v1"`},
		},
		{
			name: "EnvironmentNameDoesNotMatchFilenameStem",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Name = "prod2"
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"prod2"`, `"prod"`, "rename"},
		},
		{
			name: "EnvironmentNamesAClusterWithNoRecord",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Placement.Cluster = strPtr("ghost")
				s.Environments["wren/prod"] = e
				// checkOrphanDeliveryUnits derives the wanted delivery
				// directory from (cluster, namespace); moving to a cluster
				// with no record also moves what is wanted from
				// prod/wren-prod to ghost/wren-prod, which would otherwise
				// add two more, unrelated refusals (the old one orphaned,
				// the new one missing). Moving the fixture's delivery unit
				// to match keeps this case isolated to the cluster-reference
				// defect.
				s.DeliveryUnits = []string{"ghost/wren-prod"}
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"ghost"`, "inventory/clusters/ghost.json"},
		},
		{
			name: "EnvironmentNamesAHostWithNoRecord",
			mutate: func(s *Snapshot) {
				e := s.Environments["juni/prod"]
				e.Placement.Host = strPtr("ghost")
				s.Environments["juni/prod"] = e
			},
			wantFile:  "inventory/environments/juni/prod.json",
			wantWords: []string{`"ghost"`, "inventory/hosts/ghost.json"},
		},
		{
			name: "RequiresNotASubsetOfClusterCapabilities",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Requires = []string{"ingress", "gpu"}
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"gpu"`, "add", "place the environment on a different cluster"},
		},
		{
			name: "TwoEnvironmentsClaimTheSameClusterNamespace",
			mutate: func(s *Snapshot) {
				p := s.Projects["wren"]
				p.Environments = append(p.Environments, "staging")
				s.Projects["wren"] = p
				s.Environments["wren/staging"] = Environment{
					Schema:    environmentSchema,
					Project:   "wren",
					Name:      "staging",
					Shape:     "kubernetes",
					Placement: Placement{Cluster: strPtr("prod"), Namespace: strPtr("wren-prod")},
					Vault:     VaultRef{Mount: "secret", Prefix: "wren/staging"},
					Stateful:  boolPtr(false),
				}
				// No DeliveryUnits change needed: deliveryUnit derives the
				// directory from the NAMESPACE, and both environments claim
				// "wren-prod" -- the collision this case means to test --
				// so they derive the identical requirement, already
				// satisfied by validSnapshot's existing prod/wren-prod.
				// An earlier version of this fixture added a second,
				// same-named-but-wrong "prod/wren-staging" entry here, which
				// nothing ever wants and which checkOrphanDeliveryUnits
				// correctly flagged as stale -- a second, unrelated refusal
				// this case was not supposed to be testing.
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"inventory/environments/wren/staging.json", `"prod"`, `"wren-prod"`},
		},
		{
			name: "DeliveryUnitWithNoEnvironmentNamingIt",
			mutate: func(s *Snapshot) {
				s.DeliveryUnits = append(s.DeliveryUnits, "prod/orphan-unit")
			},
			wantFile:  "deliveries/prod/orphan-unit",
			wantWords: []string{"no environment names this delivery unit", "remove"},
		},
		{
			name: "KubernetesEnvironmentWithNoDeliveryUnit",
			mutate: func(s *Snapshot) {
				s.DeliveryUnits = nil
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"deliveries/prod/wren-prod", "create"},
		},
		{
			name: "ClusterNamesAHostWithNoRecord",
			mutate: func(s *Snapshot) {
				c := s.Clusters["prod"]
				c.Hosts = append(c.Hosts, "ghost")
				s.Clusters["prod"] = c
			},
			wantFile:  "inventory/clusters/prod.json",
			wantWords: []string{`"ghost"`, "inventory/hosts/ghost.json"},
		},
		{
			name: "HostNamesAClusterThatDoesNotListItInHosts",
			mutate: func(s *Snapshot) {
				c := s.Clusters["prod"]
				c.Hosts = nil
				s.Clusters["prod"] = c
			},
			wantFile:  "inventory/hosts/alpha.json",
			wantWords: []string{`"prod"`, "does not list", `"alpha"`},
		},
		{
			name: "HostWithConfigNullAndRoleNotUnmanaged",
			mutate: func(s *Snapshot) {
				h := s.Hosts["beta"]
				h.Role = "worker"
				s.Hosts["beta"] = h
			},
			wantFile:  "inventory/hosts/beta.json",
			wantWords: []string{`"worker"`, "unmanaged"},
		},
		{
			name: "ProjectListsAnEnvironmentWithNoRecord",
			mutate: func(s *Snapshot) {
				p := s.Projects["wren"]
				p.Environments = append(p.Environments, "ghost")
				s.Projects["wren"] = p
			},
			wantFile:  "inventory/projects/wren.json",
			wantWords: []string{`"ghost"`, "inventory/environments/wren/ghost.json"},
		},
		{
			name: "EnvironmentsProjectHasNoRecord",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Project = "ghost"
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"ghost"`, "inventory/projects/ghost.json"},
		},
		{
			name: "KubernetesShapeMissingPlacement",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Placement.Namespace = nil
				s.Environments["wren/prod"] = e
				// checkOrphanDeliveryUnits skips an environment missing
				// either half of its placement -- see its own comment -- so
				// wren/prod stops wanting ANY delivery directory here, which
				// would orphan validSnapshot's prod/wren-prod as a second,
				// unrelated refusal. Dropping it isolates this case to the
				// missing-placement defect alone.
				s.DeliveryUnits = nil
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"kubernetes", "placement.cluster", "placement.namespace"},
		},
		{
			name: "VMShapeMissingHost",
			mutate: func(s *Snapshot) {
				e := s.Environments["juni/prod"]
				e.Placement.Host = nil
				s.Environments["juni/prod"] = e
			},
			wantFile:  "inventory/environments/juni/prod.json",
			wantWords: []string{`"vm"`, "placement.host"},
		},
		{
			name: "PlacementSetsBothClusterAndHost",
			mutate: func(s *Snapshot) {
				e := s.Environments["juni/prod"]
				e.Placement.Cluster = strPtr("prod")
				s.Environments["juni/prod"] = e
			},
			wantFile:  "inventory/environments/juni/prod.json",
			wantWords: []string{"both cluster and host", "remove"},
		},
		{
			name: "VaultMountEmpty",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Vault.Mount = ""
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"vault.mount", "empty", "set vault.mount"},
		},
		{
			name: "VaultPrefixEmpty",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Vault.Prefix = ""
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"vault.prefix", "empty", "set vault.prefix"},
		},
		{
			name: "EnvironmentStatefulUnset",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Stateful = nil
				s.Environments["wren/prod"] = e
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{"unstated", "true or false", "state stateful"},
		},
		{
			name: "ClusterHasHAVaultUnset",
			mutate: func(s *Snapshot) {
				c := s.Clusters["prod"]
				c.HasHAVault = nil
				s.Clusters["prod"] = c
			},
			wantFile:  "inventory/clusters/prod.json",
			wantWords: []string{"unstated", "true or false", "state has_ha_vault"},
		},
		{
			name: "UnknownShape",
			mutate: func(s *Snapshot) {
				e := s.Environments["wren/prod"]
				e.Shape = "container"
				s.Environments["wren/prod"] = e
				// checkOrphanDeliveryUnits only wants a delivery for shape
				// "kubernetes"; an unrecognised shape stops wanting one at
				// all, which would orphan validSnapshot's prod/wren-prod as
				// a second, unrelated refusal. Dropping it isolates this
				// case to the unrecognised-shape defect alone.
				s.DeliveryUnits = nil
			},
			wantFile:  "inventory/environments/wren/prod.json",
			wantWords: []string{`"container"`, "not recognised", "fix shape"},
		},
		{
			name: "UnknownHostKind",
			mutate: func(s *Snapshot) {
				h := s.Hosts["alpha"]
				h.Kind = "cloud"
				s.Hosts["alpha"] = h
			},
			wantFile:  "inventory/hosts/alpha.json",
			wantWords: []string{`"cloud"`, "not recognised", "fix kind"},
		},
	}
}

// TestCheckRefusesEachDefect breaks exactly one thing in the valid snapshot
// per case and requires Check to report exactly one MORE problem than the
// clean baseline (zero, per TestAValidSnapshotHasNoProblems) -- not merely
// "at least one". Asserting only presence would still pass a Check that grew
// a second, spurious refusal alongside the real one: the count is the only
// thing here that catches a mutation which widens a check rather than
// disabling it.
func TestCheckRefusesEachDefect(t *testing.T) {
	baseline := len(Check(validSnapshot()))
	for _, d := range defects() {
		t.Run(d.name, func(t *testing.T) {
			s := validSnapshot()
			d.mutate(&s)
			got := Check(s)
			if len(got) != baseline+1 {
				t.Fatalf("Check returned %d problems, want exactly %d (the clean baseline plus this one defect): %v", len(got), baseline+1, got)
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, d.wantFile) {
				t.Errorf("no problem named %q; got: %v", d.wantFile, got)
			}
			for _, w := range d.wantWords {
				if !strings.Contains(joined, w) {
					t.Errorf("no problem contained %q; got: %v", w, got)
				}
			}
		})
	}
}

// TestEveryProblemNamesAFileAndAnImperative walks every problem produced
// across all the failure fixtures and requires each to name a file (or a
// deliveries/ directory) and tell the reader what to do about it. This is
// the test that keeps the error strings usable by an agent or an operator
// who has never read this package's source.
func TestEveryProblemNamesAFileAndAnImperative(t *testing.T) {
	imperatives := []string{"add", "rename", "fix", "remove", "set", "place", "state", "create", "delete"}

	for _, d := range defects() {
		s := validSnapshot()
		d.mutate(&s)
		for _, p := range Check(s) {
			if !strings.Contains(p, ".json") && !strings.Contains(p, "deliveries/") {
				t.Errorf("%s: problem %q names no file or directory path", d.name, p)
			}
			found := false
			for _, imp := range imperatives {
				if strings.Contains(p, imp) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: problem %q contains none of %v", d.name, p, imperatives)
			}
		}
	}
}

// TestCheckIsPure calls Check twice on the same snapshot and requires the
// same answer both times, and requires the snapshot itself to come out
// unchanged. Two independently built copies of the same fixture stay deeply
// equal only if Check never wrote through a pointer or shared slice inside
// the one it was handed.
func TestCheckIsPure(t *testing.T) {
	s := validSnapshot()
	untouched := validSnapshot()

	got1 := Check(s)
	got2 := Check(s)
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("Check(s) called twice gave different answers:\n%v\n%v", got1, got2)
	}
	if !reflect.DeepEqual(s, untouched) {
		t.Fatalf("Check mutated its input snapshot")
	}
}

// TestProblemsAreSorted builds a snapshot with several unrelated defects
// and requires the result to come back in sorted order.
func TestProblemsAreSorted(t *testing.T) {
	s := validSnapshot()
	for _, name := range []string{"UnknownShape", "UnknownHostKind", "ClusterHasHAVaultUnset", "VaultMountEmpty"} {
		for _, d := range defects() {
			if d.name == name {
				d.mutate(&s)
			}
		}
	}

	got := Check(s)
	if len(got) < 2 {
		t.Fatalf("expected multiple problems, got %v", got)
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("problems are not sorted: %v", got)
	}
}

// TestInventoryPerformsNoIO pins the package's import set to the doc
// comment's promise: this package takes decoded data and returns strings,
// nothing more. An import is the cheapest way that promise breaks
// silently.
func TestInventoryPerformsNoIO(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "encoding/json": true, "fmt": true, "sort": true, "strings": true,
		// io/fs and path are the loader's, and they are interfaces and
		// string handling rather than I/O. "os" stays banned, which is what
		// makes the promise real: Load cannot open a path the caller did not
		// hand it, and cannot reach outside the fs.FS it was given.
		"io/fs": true, "path": true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing internal/inventory: %v", err)
	}

	for _, pkg := range pkgs {
		for fname, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unparseable import literal %s", fname, imp.Path.Value)
				}
				if !allowed[path] {
					t.Errorf("%s imports %q: this package performs no I/O and must import nothing that does", fname, path)
				}
			}
		}
	}
}

// TestTheDeliveryUnitIsTheNamespace pins which field names the delivery
// directory. An earlier build derived it as "<project>-<name>", which the
// shared fixture cannot tell apart from the namespace because that fixture
// happens to use "wren-prod" for both. Here the namespace is deliberately
// something else, so the two derivations disagree and only one passes.
func TestTheDeliveryUnitIsTheNamespace(t *testing.T) {
	s := validSnapshot()
	e := s.Environments["wren/prod"]
	e.Placement.Namespace = strPtr("web")
	s.Environments["wren/prod"] = e
	s.DeliveryUnits = []string{"prod/web"}

	if got := Check(s); len(got) != 0 {
		t.Fatalf("a delivery unit named for the namespace was refused: %v", got)
	}
}

// TestTwoEnvironmentsCannotShareADerivedDeliveryUnit is the argument for the
// choice above rather than a second check of it.
//
// Joining two free-form names with a separator that is legal inside both is
// not a bijection: project "wren-api" with environment "prod" and project
// "wren" with environment "api-prod" both render "wren-api-prod". Put them on
// ONE cluster and the derived key collides, so the map of required delivery
// directories holds a single entry where two belong -- one environment's
// requirement disappears silently, and checkNamespaceCollisions never sees a
// thing because their namespaces are genuinely different.
//
// ⚠️ THE TWO ENVIRONMENTS MUST SHARE A CLUSTER FOR THIS TO DEMONSTRATE
// ANYTHING. An earlier draft of this test placed them on separate clusters,
// where the cluster prefix keeps the keys apart and the collision cannot
// happen -- it passed under both derivations and proved nothing.
func TestTwoEnvironmentsCannotShareADerivedDeliveryUnit(t *testing.T) {
	s := validSnapshot()
	s.Projects["wren-api"] = Project{Schema: projectSchema, Name: "wren-api", Environments: []string{"prod"}}
	s.Projects["wren"] = Project{Schema: projectSchema, Name: "wren", Environments: []string{"prod", "api-prod"}}

	s.Environments["wren-api/prod"] = Environment{
		Schema: environmentSchema, Project: "wren-api", Name: "prod", Shape: "kubernetes",
		Placement: Placement{Cluster: strPtr("prod"), Namespace: strPtr("api")},
		Requires:  []string{"ingress"}, Vault: VaultRef{Mount: "secret", Prefix: "wren-api/prod"},
		Stateful: boolPtr(false),
	}
	s.Environments["wren/api-prod"] = Environment{
		Schema: environmentSchema, Project: "wren", Name: "api-prod", Shape: "kubernetes",
		Placement: Placement{Cluster: strPtr("prod"), Namespace: strPtr("web")},
		Requires:  []string{"ingress"}, Vault: VaultRef{Mount: "secret", Prefix: "wren/api-prod"},
		Stateful: boolPtr(false),
	}
	s.DeliveryUnits = append(s.DeliveryUnits, "prod/api", "prod/web")

	// Both environments are legitimate and distinct, each with its own
	// delivery directory on the shared cluster. Nothing here is a defect.
	if got := Check(s); len(got) != 0 {
		t.Fatalf("two distinct environments on one cluster were refused: %v", got)
	}
}
