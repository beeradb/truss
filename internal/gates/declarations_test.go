package gates

import "testing"

// --- CheckDeclarations ------------------------------------------------

// TestCheckDeclarationsPassesAZeroValue: an empty slice of decls -- the zero
// value, and what a clean plan with nothing to declare produces -- must
// pass. A gate that refuses on no evidence is as broken as one that fails
// open on real evidence.
func TestCheckDeclarationsPassesAZeroValue(t *testing.T) {
	if problems := CheckDeclarations("platform", nil); len(problems) != 0 {
		t.Fatalf("zero value was refused: %v", problems)
	}
	if problems := CheckDeclarations("platform", []Declaration{}); len(problems) != 0 {
		t.Fatalf("empty slice was refused: %v", problems)
	}
}

// TestCheckDeclarationsPassesACleanPlan: resources with no provisioner and
// an ordinary type clear the gate.
func TestCheckDeclarationsPassesACleanPlan(t *testing.T) {
	decls := []Declaration{
		{Address: "terraform_data.clean", Type: "terraform_data"},
		{Address: "module.m.aws_s3_bucket.data", Type: "aws_s3_bucket"},
	}
	if problems := CheckDeclarations("platform", decls); len(problems) != 0 {
		t.Fatalf("a clean plan was refused: %v", problems)
	}
}

// TestCheckDeclarationsRefusesARootProvisioner: a provisioner on a
// root-level resource is refused, and the refusal names both the address
// and the provisioner type -- an operator reading the refusal must not have
// to go back to the plan file to learn either.
func TestCheckDeclarationsRefusesARootProvisioner(t *testing.T) {
	decls := []Declaration{
		{Address: "terraform_data.root_prov", Type: "terraform_data", Provisioners: []string{"local-exec"}},
	}
	problems := CheckDeclarations("platform", decls)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "terraform_data.root_prov") {
		t.Errorf("refusal does not name the address: %v", problems)
	}
	if !hasProblemContaining(problems, "local-exec") {
		t.Errorf("refusal does not name the provisioner type: %v", problems)
	}
}

// TestCheckDeclarationsRefusesANestedModuleProvisioner: a provisioner two
// modules deep is refused the same way, and the refusal carries the full
// module-qualified address -- CheckDeclarations trusts whatever address
// internal/plan.Declarations built rather than re-deriving nesting itself.
func TestCheckDeclarationsRefusesANestedModuleProvisioner(t *testing.T) {
	decls := []Declaration{
		{
			Address:      "module.m.module.n.terraform_data.deep",
			Type:         "terraform_data",
			Provisioners: []string{"local-exec"},
		},
	}
	problems := CheckDeclarations("platform", decls)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "module.m.module.n.terraform_data.deep") {
		t.Errorf("refusal does not name the nested address: %v", problems)
	}
}

// TestCheckDeclarationsRefusesHelmRelease: docs/design.md claims "Helm is
// refused in every form"; this is the tofu-side half docs/work-items.md
// recorded as missing.
func TestCheckDeclarationsRefusesHelmRelease(t *testing.T) {
	decls := []Declaration{
		{Address: "helm_release.app", Type: "helm_release"},
	}
	problems := CheckDeclarations("platform", decls)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem, got %v", problems)
	}
	if !hasProblemContaining(problems, "helm_release.app") {
		t.Errorf("refusal does not name the address: %v", problems)
	}
}

// TestCheckDeclarationsReportsEveryOffender: several problems in one plan
// must all be reported, not just the first -- an operator fixing refusals
// one at a time, re-planning between each, is the outcome this gate must
// not cause.
func TestCheckDeclarationsReportsEveryOffender(t *testing.T) {
	decls := []Declaration{
		{Address: "terraform_data.root_prov", Type: "terraform_data", Provisioners: []string{"local-exec"}},
		{Address: "module.m.terraform_data.inner", Type: "terraform_data", Provisioners: []string{"remote-exec"}},
		{Address: "helm_release.app", Type: "helm_release"},
		{Address: "terraform_data.clean", Type: "terraform_data"},
	}
	problems := CheckDeclarations("platform", decls)
	if len(problems) != 3 {
		t.Fatalf("want exactly three problems (two provisioners, one forbidden type), got %d: %v", len(problems), problems)
	}
	for _, want := range []string{"terraform_data.root_prov", "module.m.terraform_data.inner", "helm_release.app"} {
		if !hasProblemContaining(problems, want) {
			t.Errorf("no problem named %s: %v", want, problems)
		}
	}
}

// TestCheckDeclarationsRefusesBothAtOnce: a resource that is both a
// forbidden type AND carries a provisioner is refused twice, once for each
// -- the two checks are independent facts about the same resource.
func TestCheckDeclarationsRefusesBothAtOnce(t *testing.T) {
	decls := []Declaration{
		{Address: "helm_release.app", Type: "helm_release", Provisioners: []string{"local-exec"}},
	}
	problems := CheckDeclarations("platform", decls)
	if len(problems) != 2 {
		t.Fatalf("want exactly two problems, got %d: %v", len(problems), problems)
	}
}
