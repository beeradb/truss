// This file extracts what a plan's CONFIGURATION declares -- as opposed to
// what it changes -- so internal/gates can refuse a provisioner or a
// forbidden resource type before apply. See docs/work-items.md's "The
// provisioner / `data \"external\"` gate" for why this was deferred twice,
// and docs/threat-model.md's `provisioner` row for what it now backs.
package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/beeradb/truss/internal/gates"
)

// tfModule mirrors configuration.root_module, and -- at any depth --
// module_calls.<name>.module, in `tofu show -json` output.
//
// Measured against tofu 1.12.6, both facts load-bearing here:
//
//  1. A resource with a provisioner block carries a "provisioners" array at
//     configuration.root_module.resources[].provisioners[].type; a resource
//     with none has no "provisioners" key at all (not an empty array).
//  2. A provisioner inside a module does NOT appear under root_module.
//     resources -- that array holds only resources declared directly in the
//     root -- it appears under
//     root_module.module_calls.<name>.module.resources[], and a module
//     calling another module nests the same shape again beneath its own
//     module_calls. Reading only root_module.resources therefore fails
//     OPEN for every resource inside a module, which is most of them: the
//     platform this serves is almost entirely modules.
type tfModule struct {
	Resources   []tfResource            `json:"resources"`
	ModuleCalls map[string]tfModuleCall `json:"module_calls"`
}

// tfModuleCall is one entry of a module's module_calls map.
type tfModuleCall struct {
	Module tfModule `json:"module"`
}

// tfResource is one entry of a module's resources array.
type tfResource struct {
	Address      string          `json:"address"`
	Type         string          `json:"type"`
	Provisioners []tfProvisioner `json:"provisioners"`
}

// tfProvisioner is one entry of a resource's provisioners array.
type tfProvisioner struct {
	Type string `json:"type"`
}

// Declarations lists every resource a plan's CONFIGURATION declares, at
// every module depth, with the provisioners attached to each.
//
// It reads configuration, never planned_values or resource_changes: a
// provisioner is a configuration fact, not a resource change. A resource
// this run is not touching still has its provisioner ready to run the next
// time anything touches it, and resource_changes would not show that
// resource at all.
//
// A payload that cannot be parsed is an error, never an empty slice -- an
// empty slice reads as "this plan declares nothing", which is a gate
// passing on garbage rather than refusing to judge it.
func Declarations(planJSON []byte) ([]gates.Declaration, error) {
	var doc struct {
		Configuration *struct {
			RootModule tfModule `json:"root_module"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(planJSON, &doc); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	// encoding/json leaves Configuration nil rather than erroring when the
	// key is absent -- true of `{}` and of any JSON document that is not a
	// `tofu show -json` plan. Treating that as "zero resources declared"
	// is exactly the fail-open this function exists to refuse to commit.
	if doc.Configuration == nil {
		return nil, errors.New("plan: no configuration in plan JSON (need `tofu show -json` of a plan, not of state)")
	}

	var decls []gates.Declaration
	walkModule(doc.Configuration.RootModule, "", &decls)
	return decls, nil
}

// walkModule appends one gates.Declaration per resource in m, then recurses
// into every module call to any depth, building a readable address as it
// descends -- "module.m.terraform_data.inner" -- so a refusal names where
// the thing is, not just that it exists.
//
// module_calls is a Go map; its key order is only sorted here so two runs
// over the same plan produce Declarations in the same order, not because
// anything downstream depends on it (CheckDeclarations reports every
// offender regardless of order).
func walkModule(m tfModule, prefix string, decls *[]gates.Declaration) {
	for _, r := range m.Resources {
		d := gates.Declaration{
			Address: prefix + r.Address,
			Type:    r.Type,
		}
		for _, p := range r.Provisioners {
			d.Provisioners = append(d.Provisioners, p.Type)
		}
		*decls = append(*decls, d)
	}

	names := make([]string, 0, len(m.ModuleCalls))
	for name := range m.ModuleCalls {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		walkModule(m.ModuleCalls[name].Module, prefix+"module."+name+".", decls)
	}
}
