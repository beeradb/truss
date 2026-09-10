package repo

import (
	"regexp"
	"sort"
)

// Kind is what a directory IS, and therefore how the applier treats it: what
// binary renders or plans it, what evidence the reviewer read, and what a
// refusal means.
//
// ⚠️ A KIND IS DETECTED FROM THE TREE AND NEVER FROM CONFIGURATION. The
// alternative -- a marker file in each directory declaring its own kind --
// is the flexible design and it is the wrong one, because it moves the
// decision "what will be executed, and with which credentials" out of the
// engine and into the tree being changed. "This directory is an Ansible play
// that runs as root on every managed host" is not a deployment value.
// docs/port-plan.md draws that line as engine versus deployment values, and
// TestUnitKindsAreCompiledIn pins it.
type Kind int

const (
	// KindCredentials is the one root whose state holds token values. It
	// runs first and it is the only digest-gate exemption in the system.
	KindCredentials Kind = iota
	// KindTofu is an OpenTofu root: planned, digest-gated, applied.
	KindTofu
	// KindAnsible configures machines. It has no plan digest -- CI cannot
	// reach the hosts, so anything CI filed would be a function of the
	// commit alone, which is a check that cannot fail.
	KindAnsible
	// KindRender is a Kustomize directory. The applier renders it and
	// compares bytes; a reconciler applies it.
	KindRender
)

func (k Kind) String() string {
	switch k {
	case KindCredentials:
		return "credentials"
	case KindTofu:
		return "tofu"
	case KindAnsible:
		return "ansible"
	case KindRender:
		return "render"
	}
	return "unknown"
}

// Unit is one directory the applier acts on, and how.
type Unit struct {
	Kind Kind
	Path string
}

// The unit patterns, all anchored at the start of the path for the reason
// sharedInput already gives: a commit touching "docs/deliveries/README.md"
// must not be read as touching a delivery.
// ⚠️ EVERY PATTERN REQUIRES A TRAILING SLASH, WHICH IS WHAT MAKES IT MATCH A
// DIRECTORY RATHER THAN A NAME. An earlier draft ended them with `(/|$)` so
// that one pattern could serve both a changed file and a bare unit path, and
// it read the FILE "deliveries/beta/kustomization.yaml" as a unit called
// "kustomization.yaml" on cluster beta -- and "clusters/README.md" as a
// cluster root named README.md. Caught by TestADeliveryNeedsBothSegments
// before it shipped.
//
// KindOf appends the slash instead, which is the idiom projectPath already
// uses (roots.go:19, matched against path+"/"). One shape, two callers.
var (
	clusterUnit  = regexp.MustCompile(`^clusters/([^/]+)/`)
	hostUnit     = regexp.MustCompile(`^hosts/([^/]+)/`)
	ansibleUnit  = regexp.MustCompile(`^ansible/plays/([^/]+)/`)
	baselineUnit = regexp.MustCompile(`^baselines/([^/]+)/`)
	deliveryUnit = regexp.MustCompile(`^deliveries/([^/]+)/([^/]+)/`)
)

// unitSharedInput is a path that is an input to EVERY unit.
//
// ⚠️ IT IS DELIBERATELY NOT sharedInput, EVEN THOUGH IT CONTAINS IT.
// TouchedRoots reproduces derive_touched_roots byte for byte and the parity
// corpus compares against recordings of the bash; widening the pattern it
// reads would change what that function returns for commits the corpus
// already has answers for. So the unit layer gets its own, and TouchedRoots
// keeps its exact body.
//
// inventory/ is here because its readers are not derivable from its path:
// inventory/clusters/beta.json is read by clusters/beta, by every delivery
// on beta, and by every environment placed there. Narrowing this to the
// units a given file "belongs to" is the tempting optimisation and it is
// wrong the first time a file has two readers -- the failure mode is a host
// whose play never re-ran after its own inventory entry changed, which is
// absent read as compliant.
var unitSharedInput = regexp.MustCompile(`^(modules/|inventory/|providers\.allow$|\.opentofu-version$|\.kustomize-version$|\.ansible-version$)`)

// KindOf reports what kind of unit path is, if it is a unit at all. It is
// the single place a directory's meaning is decided, so the tree listing and
// the commit diff can never disagree about what something is.
func KindOf(path string) (Kind, bool) {
	switch {
	case path == "credentials":
		return KindCredentials, true
	case path == "platform":
		return KindTofu, true
	case projectPath.MatchString(path + "/"):
		return KindTofu, true
	case clusterUnit.MatchString(path + "/"), hostUnit.MatchString(path + "/"):
		return KindTofu, true
	case ansibleUnit.MatchString(path + "/"):
		return KindAnsible, true
	case baselineUnit.MatchString(path + "/"), deliveryUnit.MatchString(path + "/"):
		return KindRender, true
	}
	return 0, false
}

// TouchedUnits derives every unit a commit touches, in the order the applier
// must act on them.
//
// The order is by kind, then by path: credentials, then tofu, then ansible,
// then render. That is a TOTAL ORDER BETWEEN KINDS and not a dependency
// graph, and the distinction matters -- docs/work-items.md refuses declared
// edges between roots on the grounds that the current failure mode is safe
// and edges would let a mis-ordered apply succeed instead of refuse. Nothing
// here lets that happen: the sequence is fixed, compiled in, and identical
// for every commit. It is also the natural order rather than an invented
// one -- infrastructure makes the machine, configuration configures it,
// delivery ships onto it.
//
// treeUnits is every unit present in the commit's own tree, supplied by
// whatever read the tree; this function does no filesystem or git I/O of its
// own, the same rule TouchedRoots keeps.
func TouchedUnits(changedFiles, treeUnits []string) []Unit {
	shared := false
	credentials := false
	for _, f := range changedFiles {
		if unitSharedInput.MatchString(f) {
			shared = true
		}
		if credentialsPath.MatchString(f) {
			credentials = true
		}
	}

	paths := make(map[string]bool)
	if credentials {
		paths["credentials"] = true
	}

	if shared {
		// A shared input plans everything that exists, which is blunt on
		// purpose: over-planning costs a slow pass, under-planning means a
		// unit built on a fact that is no longer true.
		for _, u := range treeUnits {
			if _, ok := KindOf(u); ok {
				paths[u] = true
			}
		}
		return sortUnits(paths)
	}

	for _, f := range changedFiles {
		if f == "platform" || platformPath.MatchString(f) {
			paths["platform"] = true
		}
		if m := projectPath.FindStringSubmatch(f); m != nil {
			paths["projects/"+m[1]] = true
		}
		if m := clusterUnit.FindStringSubmatch(f); m != nil {
			paths["clusters/"+m[1]] = true
		}
		if m := hostUnit.FindStringSubmatch(f); m != nil {
			paths["hosts/"+m[1]] = true
		}
		if m := ansibleUnit.FindStringSubmatch(f); m != nil {
			paths["ansible/plays/"+m[1]] = true
		}
		if m := baselineUnit.FindStringSubmatch(f); m != nil {
			paths["baselines/"+m[1]] = true
		}
		if m := deliveryUnit.FindStringSubmatch(f); m != nil {
			paths["deliveries/"+m[1]+"/"+m[2]] = true
		}
	}
	return sortUnits(paths)
}

// sortUnits turns the set into the fixed kind-then-path order.
func sortUnits(paths map[string]bool) []Unit {
	units := make([]Unit, 0, len(paths))
	for p := range paths {
		k, ok := KindOf(p)
		if !ok {
			// Unreachable: nothing is added to the set without KindOf
			// having accepted it. Dropping it rather than guessing a kind
			// keeps "a directory of unknown kind is never executed" true by
			// construction.
			continue
		}
		units = append(units, Unit{Kind: k, Path: p})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Kind != units[j].Kind {
			return units[i].Kind < units[j].Kind
		}
		return units[i].Path < units[j].Path
	})
	return units
}
