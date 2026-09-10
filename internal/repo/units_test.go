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
