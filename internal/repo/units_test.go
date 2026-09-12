package repo

import (
	"reflect"
	"testing"
)

func TestTouchedUnitsOrdersByKindThenPath(t *testing.T) {
	got := TouchedUnits([]string{
		"deliveries/beta/web/kustomization.yaml",
		"ansible/plays/k3s-server/play.yml",
		"projects/wren/main.tf",
		"credentials/cloudflare.tf",
		"clusters/beta/main.tf",
		"baselines/prod/kustomization.yaml",
		"platform/dns.tf",
		"hosts/dev-beta/main.tf",
	}, nil)

	want := []Unit{
		{KindCredentials, "credentials"},
		{KindTofu, "clusters/beta"},
		{KindTofu, "hosts/dev-beta"},
		{KindTofu, "platform"},
		{KindTofu, "projects/wren"},
		{KindAnsible, "ansible/plays/k3s-server"},
		{KindRender, "baselines/prod"},
		{KindRender, "deliveries/beta/web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits =\n%v\nwant\n%v", got, want)
	}
}

// TestInventoryIsASharedInputForUnitsButNotForRoots is the seam between the
// two functions, and it is the one that protects the parity corpus.
//
// TouchedRoots reproduces derive_touched_roots byte for byte and is compared
// against recordings of the bash. If inventory/ had simply been added to the
// pattern TouchedRoots reads, every recorded scenario whose diff touches
// inventory would start returning a different set of roots than the
// recording says -- silently, and for a reason unrelated to the change being
// tested.
func TestInventoryIsASharedInputForUnitsButNotForRoots(t *testing.T) {
	changed := []string{"inventory/clusters/beta.json"}
	tree := []string{"platform", "projects/wren", "deliveries/beta/web"}

	if got := TouchedRoots(changed, tree); len(got) != 0 {
		t.Errorf("TouchedRoots(%v) = %v, want none: widening this breaks the parity corpus", changed, got)
	}

	got := TouchedUnits(changed, tree)
	want := []Unit{
		{KindTofu, "platform"},
		{KindTofu, "projects/wren"},
		{KindRender, "deliveries/beta/web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits =\n%v\nwant every unit in the tree\n%v", got, want)
	}
}

// TestSharedInputTouchedAgreesWithTouchedUnitsWidening pins the exported
// predicate against the behaviour it exists to explain: whenever a shared
// input makes TouchedUnits widen to the whole tree, SharedInputTouched must
// say so, and whenever it does not, SharedInputTouched must not either --
// same input, same regex, no daylight between them.
func TestSharedInputTouchedAgreesWithTouchedUnitsWidening(t *testing.T) {
	tree := []string{"platform", "projects/wren"}

	shared := []string{"inventory/clusters/beta.json"}
	if !SharedInputTouched(shared) {
		t.Errorf("SharedInputTouched(%v) = false, want true", shared)
	}
	if got := TouchedUnits(shared, tree); len(got) != len(tree) {
		t.Errorf("TouchedUnits(%v, %v) = %v, want every unit in the tree (SharedInputTouched said this was shared)", shared, tree, got)
	}

	notShared := []string{"projects/wren/main.tf"}
	if SharedInputTouched(notShared) {
		t.Errorf("SharedInputTouched(%v) = true, want false", notShared)
	}
	if got := TouchedUnits(notShared, tree); len(got) != 1 {
		t.Errorf("TouchedUnits(%v, %v) = %v, want just the one unit named in the diff", notShared, tree, got)
	}
}

// TestUnitKindsAreCompiledIn pins that a directory cannot declare what it is.
// TestRootsAreNotConfigurable stops the ENVIRONMENT naming a root; this stops
// the TREE naming a kind, which is the same property one layer along: what
// gets executed, and with which credentials, is a fact about the engine and
// not about the commit being applied.
func TestUnitKindsAreCompiledIn(t *testing.T) {
	changed := []string{
		"somewhere/unit.json",
		"somewhere/kustomization.yaml",
		"somewhere/main.tf",
		"somewhere/play.yml",
	}
	if got := TouchedUnits(changed, nil); len(got) != 0 {
		t.Fatalf("TouchedUnits(%v) = %v, want none: a directory may not declare its own kind", changed, got)
	}
}

func TestPathsThatMerelyContainAUnitNameAreIgnored(t *testing.T) {
	changed := []string{
		"docs/deliveries/beta/web/README.md",
		"docs/ansible/plays/k3s/notes.md",
		"vendor/clusters/beta/thing.tf",
		"README-baselines/prod.md",
	}
	if got := TouchedUnits(changed, nil); len(got) != 0 {
		t.Fatalf("TouchedUnits(%v) = %v, want none: every pattern is anchored", changed, got)
	}
}

func TestADeepFileYieldsItsUnitOnce(t *testing.T) {
	got := TouchedUnits([]string{
		"deliveries/beta/web/base/deployment.yaml",
		"deliveries/beta/web/kustomization.yaml",
	}, nil)
	want := []Unit{{KindRender, "deliveries/beta/web"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits = %v, want %v", got, want)
	}
}

// TestADeliveryNeedsBothSegments guards the one pattern with two captures. A
// file directly under deliveries/<cluster>/ names no unit, and reading it as
// one would render a directory that is not a delivery.
func TestADeliveryNeedsBothSegments(t *testing.T) {
	if got := TouchedUnits([]string{"deliveries/beta/kustomization.yaml"}, nil); len(got) != 0 {
		t.Fatalf("TouchedUnits = %v, want none for a file directly under a cluster", got)
	}
}

func TestKindOfRejectsAnUnknownPath(t *testing.T) {
	for _, p := range []string{"", "docs", "modules/vpc", "deliveries/beta", "ansible/roles/base", "scripts"} {
		if k, ok := KindOf(p); ok {
			t.Errorf("KindOf(%q) = %v, true; want it rejected", p, k)
		}
	}
}

func TestASharedInputWithCredentialsIncludesItFirstForUnits(t *testing.T) {
	got := TouchedUnits(
		[]string{"modules/vpc/main.tf", "credentials/cloudflare.tf"},
		[]string{"deliveries/beta/web", "platform"},
	)
	want := []Unit{
		{KindCredentials, "credentials"},
		{KindTofu, "platform"},
		{KindRender, "deliveries/beta/web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedUnits =\n%v\nwant\n%v", got, want)
	}
}

// TestAFileDirectlyUnderAUnitPrefixIsNotAUnit is the sibling of
// TestADeliveryNeedsBothSegments, for every other pattern. A file sitting
// beside the unit directories -- a README, a shared kustomization -- names no
// unit, and reading it as one would plan or render something that is not
// there.
func TestAFileDirectlyUnderAUnitPrefixIsNotAUnit(t *testing.T) {
	for _, f := range []string{
		"clusters/README.md",
		"hosts/README.md",
		"ansible/plays/README.md",
		"baselines/README.md",
		"deliveries/README.md",
	} {
		if got := TouchedUnits([]string{f}, nil); len(got) != 0 {
			t.Errorf("TouchedUnits([%q]) = %v, want none", f, got)
		}
	}
}

// TestTouchedUnitsTofuHalfMatchesTouchedRoots is the property that makes
// runCommitLoop's swap from repo.TouchedRoots to the credentials+tofu half
// of repo.TouchedUnits safe: for a commit whose changed files and tree
// contain no clusters/<name>, no hosts/<name>, and nothing that matches
// unitSharedInput but not sharedInput (inventory/, .kustomize-version,
// .ansible-version -- the three patterns unitSharedInput has and
// sharedInput does not), the two functions must return the identical root
// set, in the identical order.
//
// This is what internal/parity's 43 recorded scenarios have always been, so
// pinning it here is what lets the swap happen without widening what the
// bash-parity corpus already answers for. Widening unitSharedInput later
// without touching this test would be the failure mode this guards against.
func TestTouchedUnitsTofuHalfMatchesTouchedRoots(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		tree    []string // fed to both TouchedRoots and TouchedUnits verbatim
	}{
		{
			name:    "a single project root",
			changed: []string{"projects/recipes/main.tf"},
		},
		{
			name:    "platform and a project together",
			changed: []string{"platform/main.tf", "projects/recipes/main.tf"},
		},
		{
			name:    "credentials alone",
			changed: []string{"credentials/cloudflare.tf"},
		},
		{
			name:    "credentials with a project",
			changed: []string{"credentials/cloudflare.tf", "projects/recipes/main.tf"},
		},
		{
			name:    "a shared tofu input plans the whole tree",
			changed: []string{"modules/vpc/main.tf"},
			tree:    []string{"platform", "projects/alpha", "projects/beta"},
		},
		{
			name:    "the provider allowlist is a shared input",
			changed: []string{"providers.allow"},
			tree:    []string{"platform"},
		},
		{
			name:    "the pinned opentofu version is a shared input",
			changed: []string{".opentofu-version"},
			tree:    []string{"projects/alpha"},
		},
		{
			name:    "a shared input alongside credentials",
			changed: []string{"modules/vpc/main.tf", "credentials/cloudflare.tf"},
			tree:    []string{"platform"},
		},
		{
			name:    "a path outside every root or unit",
			changed: []string{"docs/README.md"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantRoots := TouchedRoots(c.changed, c.tree)

			var got []string
			for _, u := range TouchedUnits(c.changed, c.tree) {
				if u.Kind == KindCredentials || u.Kind == KindTofu {
					got = append(got, u.Path)
				}
			}

			if !reflect.DeepEqual(got, wantRoots) {
				t.Fatalf("credentials+tofu half of TouchedUnits(%v, %v) = %v, want TouchedRoots' answer %v", c.changed, c.tree, got, wantRoots)
			}
		})
	}
}

// TestASubdirectoryInsideAUnitIsNotItselfAUnit is the regression for the
// wedge of 2026-09-12. KindOf classified by PREFIX, so every directory NESTED
// inside a unit answered "yes, I am a unit too". Ansible's own standard
// layout was the first to commit one: ansible/plays/<play>/group_vars became
// a play in its own right -- with no site.yml and no host naming it -- and
// the target gate refused every play in the pass, every five minutes, until
// the engine was fixed.
//
// ⚠️ IT WAS NOT ANSIBLE-SPECIFIC AND THE OTHER KINDS ARE LISTED HERE ON
// PURPOSE. The same prefix rule accepted hosts/<h>/anything and
// deliveries/<c>/<n>/anything; nobody had committed a subdirectory inside
// those yet. A delivery's own base/ overlay would have found it next.
func TestASubdirectoryInsideAUnitIsNotItselfAUnit(t *testing.T) {
	for _, p := range []string{
		"ansible/plays/dev-workstation/group_vars",
		"ansible/plays/dev-workstation/host_vars",
		"ansible/plays/node-exporter/group_vars/nested/deeper",
		"clusters/beta/manifests",
		"hosts/h/.terraform",
		"baselines/base/overlays",
		"deliveries/beta/web/base",
	} {
		if k, ok := KindOf(p); ok {
			t.Errorf("KindOf(%q) = %v, true; want it rejected -- it is a directory INSIDE a unit, not a unit", p, k)
		}
	}

	// And the units themselves must still be units, or the fix has simply
	// stopped truss seeing anything at all.
	for p, want := range map[string]Kind{
		"ansible/plays/dev-workstation": KindAnsible,
		"clusters/beta":                 KindTofu,
		"hosts/h":                       KindTofu,
		"baselines/base":                KindRender,
		"deliveries/beta/web":           KindRender,
		"platform":                      KindTofu,
		"credentials":                   KindCredentials,
	} {
		k, ok := KindOf(p)
		if !ok || k != want {
			t.Errorf("KindOf(%q) = %v, %v; want %v, true", p, k, ok, want)
		}
	}
}

// TestAFileInAUnitsSubdirectoryStillSelectsThatUnit is the other half, and it
// exists to stop the fix above being made the wrong way. The same patterns
// map a CHANGED FILE to the unit that owns it, and there the prefix shape is
// correct: a file in group_vars/ belongs to its play. Anchoring the shared
// patterns would fix classification and silently break selection -- commits
// touching a play's variables would select no play at all, which is the
// "absent read as compliant" failure the whole unit machinery exists to
// avoid, and a quieter one than the wedge it replaced.
func TestAFileInAUnitsSubdirectoryStillSelectsThatUnit(t *testing.T) {
	for _, tc := range []struct {
		file string
		want Unit
	}{
		{"ansible/plays/dev-workstation/group_vars/all.yml", Unit{KindAnsible, "ansible/plays/dev-workstation"}},
		{"ansible/plays/dev-workstation/host_vars/dev-agent.yml", Unit{KindAnsible, "ansible/plays/dev-workstation"}},
		{"hosts/h/nested/main.tf", Unit{KindTofu, "hosts/h"}},
		{"deliveries/beta/web/base/kustomization.yaml", Unit{KindRender, "deliveries/beta/web"}},
	} {
		got := TouchedUnits([]string{tc.file}, nil)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("TouchedUnits(%q) = %v; want exactly [%v]", tc.file, got, tc.want)
		}
	}
}
