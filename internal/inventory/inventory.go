package inventory

import (
	"fmt"
	"sort"
	"strings"
)

// Snapshot is the whole inventory, already decoded. The keys are the
// filename stems the records were read from, so Check can verify that a
// record's own Name field agrees with where it was found.
type Snapshot struct {
	Hosts        map[string]Host
	Clusters     map[string]Cluster
	Projects     map[string]Project
	Environments map[string]Environment // key is "<project>/<name>"
	// DeliveryUnits is the set of deliveries/<cluster>/<unit> directories
	// present in the tree, as "<cluster>/<unit>". It comes from the caller
	// reading the tree; this package does no I/O.
	DeliveryUnits []string
}

func hostPath(stem string) string       { return fmt.Sprintf("inventory/hosts/%s.json", stem) }
func clusterPath(stem string) string    { return fmt.Sprintf("inventory/clusters/%s.json", stem) }
func projectPath(stem string) string    { return fmt.Sprintf("inventory/projects/%s.json", stem) }
func environmentPath(key string) string { return fmt.Sprintf("inventory/environments/%s.json", key) }
func deliveryPath(cluster, unit string) string {
	return fmt.Sprintf("deliveries/%s/%s", cluster, unit)
}

// deliveryUnit is the on-disk name the delivery for one environment must
// carry. There is no field on Environment for it -- unlike a cluster or a
// host, a delivery unit is not itself an inventory record, only a directory
// -- so the name is derived from a field that already uniquely identifies
// the environment on its cluster: the namespace it deploys into.
//
// ⚠️ IT IS THE NAMESPACE AND NOT "<project>-<name>", AND THE REASON IS AN
// AMBIGUITY RATHER THAN A PREFERENCE. Joining two free-form names with a
// separator that is legal inside both of them is not a bijection: project
// "wren-api" with environment "prod" and project "wren" with environment
// "api-prod" both render "wren-api-prod". Two different environments would
// claim one directory, and nothing downstream could tell which one a render
// belonged to -- on different clusters, or with different namespaces, they
// would not even collide anywhere else first.
//
// The namespace has none of that trouble, and the uniqueness is not a new
// assumption: checkNamespaceCollisions already refuses two environments
// sharing one (cluster, namespace), so "deliveries/<cluster>/<namespace>"
// is unique by an invariant this package was already enforcing for its own
// reasons. Deriving it from something already proven unique is strictly
// better than deriving it from something merely expected to be.
//
// Callers must only reach this for an environment whose placement carries a
// namespace; checkEnvironment refuses a kubernetes shape without one, and
// checkOrphanDeliveryUnits skips any environment that failed that check.
func deliveryUnit(e Environment) string { return *e.Placement.Namespace }

// Check returns every problem with the snapshot, sorted, or an empty slice
// when the inventory is consistent. It does no I/O and does not mutate s:
// every map it reads is read only, and nothing here writes back into a
// Host, Cluster, Project or Environment value.
func Check(s Snapshot) []string {
	var problems []string

	for stem, h := range s.Hosts {
		problems = append(problems, checkHost(s, stem, h)...)
	}
	for stem, c := range s.Clusters {
		problems = append(problems, checkCluster(s, stem, c)...)
	}
	for stem, p := range s.Projects {
		problems = append(problems, checkProject(s, stem, p)...)
	}
	for key, e := range s.Environments {
		problems = append(problems, checkEnvironment(s, key, e)...)
	}

	problems = append(problems, checkNamespaceCollisions(s)...)
	problems = append(problems, checkOrphanDeliveryUnits(s)...)

	sort.Strings(problems)
	return problems
}

// --- hosts ------------------------------------------------------------

func checkHost(s Snapshot, stem string, h Host) []string {
	var problems []string
	path := hostPath(stem)

	if h.Schema != hostSchema {
		problems = append(problems, fmt.Sprintf(
			"%s: schema %q is not recognised — this build understands %q -- fix the schema field",
			path, h.Schema, hostSchema))
	}
	if h.Name != stem {
		problems = append(problems, fmt.Sprintf(
			"%s: name is %q but the file is named %q — rename the file or fix the field",
			path, h.Name, stem))
	}
	if h.Kind != "vm" && h.Kind != "bare-metal" {
		problems = append(problems, fmt.Sprintf(
			"%s: kind %q is not recognised — fix kind to \"vm\" or \"bare-metal\"", path, h.Kind))
	}
	// A machine with a role and no configuration is an omission, not a
	// choice: "unmanaged" is the only role that means truss was never
	// asked to configure this host, and every other role names a job the
	// host is supposed to be doing with nothing on record saying how.
	if h.Config == nil && h.Role != "unmanaged" {
		problems = append(problems, fmt.Sprintf(
			"%s: config is null but role is %q, not \"unmanaged\" — set role to \"unmanaged\" or set a config path",
			path, h.Role))
	}
	if h.Cluster != nil {
		cluster, ok := s.Clusters[*h.Cluster]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: cluster names %q, which has no %s record — add the cluster record or fix the host's cluster field",
				path, *h.Cluster, clusterPath(*h.Cluster)))
		} else if !contains(cluster.Hosts, stem) {
			problems = append(problems, fmt.Sprintf(
				"%s: cluster names %q, but %s does not list %q in hosts — add %q to the cluster's hosts list, or fix the host's cluster field",
				path, *h.Cluster, clusterPath(*h.Cluster), stem, stem))
		}
	}
	return problems
}

// --- clusters -----------------------------------------------------------

func checkCluster(s Snapshot, stem string, c Cluster) []string {
	var problems []string
	path := clusterPath(stem)

	if c.Schema != clusterSchema {
		problems = append(problems, fmt.Sprintf(
			"%s: schema %q is not recognised — this build understands %q -- fix the schema field",
			path, c.Schema, clusterSchema))
	}
	if c.Name != stem {
		problems = append(problems, fmt.Sprintf(
			"%s: name is %q but the file is named %q — rename the file or fix the field",
			path, c.Name, stem))
	}
	// Unstated is not "no", and it is not "yes": a cluster whose Vault
	// availability was never recorded is a real cluster somebody has to
	// go and check, not a default. Both true and false are answers Check
	// accepts; only nil is refused.
	if c.HasHAVault == nil {
		problems = append(problems, fmt.Sprintf(
			"%s: has_ha_vault is unstated — state has_ha_vault as true or false; an unanswered field must not be read as either",
			path))
	}
	for _, hostName := range c.Hosts {
		if _, ok := s.Hosts[hostName]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: hosts names %q, which has no %s record — add the host record or remove %q from the cluster's hosts list",
				path, hostName, hostPath(hostName), hostName))
		}
	}
	return problems
}

// --- projects -------------------------------------------------------------

func checkProject(s Snapshot, stem string, p Project) []string {
	var problems []string
	path := projectPath(stem)

	if p.Schema != projectSchema {
		problems = append(problems, fmt.Sprintf(
			"%s: schema %q is not recognised — this build understands %q -- fix the schema field",
			path, p.Schema, projectSchema))
	}
	if p.Name != stem {
		problems = append(problems, fmt.Sprintf(
			"%s: name is %q but the file is named %q — rename the file or fix the field",
			path, p.Name, stem))
	}
	for _, envName := range p.Environments {
		key := p.Name + "/" + envName
		if _, ok := s.Environments[key]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: environments names %q, which has no %s record — add the environment record or remove %q from the project's environments list",
				path, envName, environmentPath(key), envName))
		}
	}
	return problems
}

// --- environments -----------------------------------------------------------

func checkEnvironment(s Snapshot, key string, e Environment) []string {
	var problems []string
	path := environmentPath(key)
	stem := key[strings.LastIndex(key, "/")+1:]

	if e.Schema != environmentSchema {
		problems = append(problems, fmt.Sprintf(
			"%s: schema %q is not recognised — this build understands %q -- fix the schema field",
			path, e.Schema, environmentSchema))
	}
	if e.Name != stem {
		problems = append(problems, fmt.Sprintf(
			"%s: name is %q but the file is named %q — rename the file or fix the field",
			path, e.Name, stem))
	}
	if _, ok := s.Projects[e.Project]; !ok {
		problems = append(problems, fmt.Sprintf(
			"%s: project names %q, which has no %s record — add the project record or fix the environment's project field",
			path, e.Project, projectPath(e.Project)))
	}

	switch e.Shape {
	case "kubernetes":
		if e.Placement.Cluster == nil || e.Placement.Namespace == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: shape is \"kubernetes\" but placement.cluster or placement.namespace is unset — set both placement.cluster and placement.namespace",
				path))
		}
	case "vm":
		if e.Placement.Host == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: shape is \"vm\" but placement.host is unset — set placement.host",
				path))
		}
	default:
		problems = append(problems, fmt.Sprintf(
			"%s: shape %q is not recognised — fix shape to \"kubernetes\" or \"vm\"", path, e.Shape))
	}

	if e.Placement.Cluster != nil && e.Placement.Host != nil {
		problems = append(problems, fmt.Sprintf(
			"%s: placement sets both cluster and host — remove one of placement.cluster or placement.host, an environment runs in exactly one place",
			path))
	}

	if e.Placement.Cluster != nil {
		cluster, ok := s.Clusters[*e.Placement.Cluster]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: placement.cluster names %q, which has no %s record — add the cluster record or fix placement.cluster",
				path, *e.Placement.Cluster, clusterPath(*e.Placement.Cluster)))
		} else {
			for _, need := range e.Requires {
				if !contains(cluster.Capabilities, need) {
					problems = append(problems, fmt.Sprintf(
						"%s: requires %q, which cluster %q does not have in capabilities — add %q to the cluster's capabilities, or place the environment on a different cluster",
						path, need, *e.Placement.Cluster, need))
				}
			}
		}
	}

	if e.Placement.Host != nil {
		if _, ok := s.Hosts[*e.Placement.Host]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: placement.host names %q, which has no %s record — add the host record or fix placement.host",
				path, *e.Placement.Host, hostPath(*e.Placement.Host)))
		}
	}

	if e.Vault.Mount == "" {
		problems = append(problems, fmt.Sprintf(
			"%s: vault.mount is empty — set vault.mount", path))
	}
	if e.Vault.Prefix == "" {
		problems = append(problems, fmt.Sprintf(
			"%s: vault.prefix is empty — set vault.prefix", path))
	}

	return problems
}

// --- checks that compare across the whole snapshot -----------------------

// checkNamespaceCollisions refuses two environments that place themselves
// in the same (cluster, namespace). Nothing about a single Environment
// record can catch this -- it is a property of the pair -- so it is
// checked once here rather than folded into checkEnvironment, which sees
// one record at a time.
func checkNamespaceCollisions(s Snapshot) []string {
	claims := make(map[string][]string) // "cluster/namespace" -> environment keys, sorted below

	keys := make([]string, 0, len(s.Environments))
	for key := range s.Environments {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		e := s.Environments[key]
		if e.Placement.Cluster == nil || e.Placement.Namespace == nil {
			continue
		}
		claim := *e.Placement.Cluster + "/" + *e.Placement.Namespace
		claims[claim] = append(claims[claim], key)
	}

	var problems []string
	claimNames := make([]string, 0, len(claims))
	for claim := range claims {
		claimNames = append(claimNames, claim)
	}
	sort.Strings(claimNames)

	for _, claim := range claimNames {
		envKeys := claims[claim]
		if len(envKeys) < 2 {
			continue
		}
		var paths []string
		for _, k := range envKeys {
			paths = append(paths, environmentPath(k))
		}
		parts := strings.SplitN(claim, "/", 2)
		problems = append(problems, fmt.Sprintf(
			"two environments claim the same placement (cluster %q, namespace %q): %s — set a different namespace for one of them, or place one of them on a different cluster",
			parts[0], parts[1], strings.Join(paths, " and ")))
	}
	return problems
}

// checkOrphanDeliveryUnits checks both directions between environments and
// deliveries/<cluster>/<unit> directories. A one-directional check is how
// a workload gets left running on a cluster it was moved off: if only
// "every kubernetes environment has a delivery" were checked, deleting the
// environment record while forgetting the directory would pass silently,
// and the workload keeps running with nothing in the inventory admitting
// it exists.
func checkOrphanDeliveryUnits(s Snapshot) []string {
	want := make(map[string]string) // "cluster/unit" -> environment path, for kubernetes environments
	keys := make([]string, 0, len(s.Environments))
	for key := range s.Environments {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		e := s.Environments[key]
		// A kubernetes environment missing either half of its placement has
		// already been refused by checkEnvironment, and there is nothing
		// useful to say about its delivery directory on top of that: the
		// unit name is derived from the namespace, so without one there is
		// no name to look for. Skipping here keeps the second refusal from
		// being a nil dereference rather than a message.
		if e.Shape != "kubernetes" || e.Placement.Cluster == nil || e.Placement.Namespace == nil {
			continue
		}
		want[*e.Placement.Cluster+"/"+deliveryUnit(e)] = environmentPath(key)
	}

	have := make(map[string]bool, len(s.DeliveryUnits))
	for _, d := range s.DeliveryUnits {
		have[d] = true
	}

	var problems []string

	wantNames := make([]string, 0, len(want))
	for w := range want {
		wantNames = append(wantNames, w)
	}
	sort.Strings(wantNames)
	for _, w := range wantNames {
		if !have[w] {
			parts := strings.SplitN(w, "/", 2)
			problems = append(problems, fmt.Sprintf(
				"%s: shape is \"kubernetes\" with no matching %s — create the delivery directory or remove the environment",
				want[w], deliveryPath(parts[0], parts[1])))
		}
	}

	units := append([]string(nil), s.DeliveryUnits...)
	sort.Strings(units)
	for _, d := range units {
		if _, ok := want[d]; !ok {
			parts := strings.SplitN(d, "/", 2)
			problems = append(problems, fmt.Sprintf(
				"%s: no environment names this delivery unit — remove the stale directory or add the environment that owns it",
				deliveryPath(parts[0], parts[1])))
		}
	}
	return problems
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
