// Package inventory validates the hand-authored records that say what
// truss's infrastructure IS -- which hosts exist, which clusters they form,
// which projects and environments run on them -- as distinct from
// internal/gates, which decides whether a CHANGE to that infrastructure may
// be applied. Nothing here performs I/O: every function takes data somebody
// else already read from the tree and decoded, and returns a list of
// problems. That is what makes "an environment naming a cluster that does
// not exist is a refusal" a test that builds a value and calls a function,
// rather than one that has to lay out a directory tree on disk first.
package inventory

// The four schema versions this build understands. A record's schema field
// is the only thing that says which shape follows; accepting anything else
// means guessing at a shape a future version may have changed underneath a
// name this one still recognises.
const (
	hostSchema        = "truss.host/v1"
	clusterSchema     = "truss.cluster/v1"
	projectSchema     = "truss.project/v1"
	environmentSchema = "truss.environment/v1"
)

// Host is one machine truss may configure.
type Host struct {
	Schema         string   `json:"schema"` // must be exactly "truss.host/v1"
	Name           string   `json:"name"`   // must equal the filename stem
	Kind           string   `json:"kind"`   // "vm" | "bare-metal"
	Role           string   `json:"role"`
	ProvisionedBy  string   `json:"provisioned_by"` // a tofu unit path, e.g. "hosts/dev-beta"
	Config         *string  `json:"config"`         // an ansible unit path, or explicit null
	TailnetTags    []string `json:"tailnet_tags"`
	Cluster        *string  `json:"cluster"` // cluster name, or explicit null
	Frozen         bool     `json:"frozen"`
	Decommissioned bool     `json:"decommissioned"`
}

// Cluster is one Kubernetes cluster.
type Cluster struct {
	Schema         string   `json:"schema"` // "truss.cluster/v1"
	Name           string   `json:"name"`
	Distribution   string   `json:"distribution"` // e.g. "k3s"
	Hosts          []string `json:"hosts"`        // host names
	Baseline       string   `json:"baseline"`     // a baselines/<name> directory
	Capabilities   []string `json:"capabilities"`
	KubeconfigItem string   `json:"kubeconfig_item"` // a vault ITEM NAME, never a value
	// HasHAVault is tri-state on purpose. A cluster that has not stated
	// whether its Vault is highly available is a different fact from one
	// that has stated "no" -- an unattended single-node Vault is a real
	// outage risk this inventory exists to surface, and a bool defaulting
	// nil to false would report every never-asked cluster as having
	// already answered "no", which is not a fact anyone recorded. Check
	// refuses the nil case rather than reading it either way.
	HasHAVault *bool `json:"has_ha_vault"`
}

// Project groups environments and declares what repos it owns.
type Project struct {
	Schema       string   `json:"schema"` // "truss.project/v1"
	Name         string   `json:"name"`
	Environments []string `json:"environments"`
}

// Environment is one deployment of one project.
type Environment struct {
	Schema    string    `json:"schema"` // "truss.environment/v1"
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	Shape     string    `json:"shape"` // "kubernetes" | "vm"
	Placement Placement `json:"placement"`
	Requires  []string  `json:"requires"`
	Vault     VaultRef  `json:"vault"`
	Frozen    bool      `json:"frozen"`
	// Stateful is tri-state for the same reason Cluster.HasHAVault is: a
	// workload that has never said whether it holds persistent state (a
	// PVC, a database, anything that does not follow the delivery unit) is
	// a different fact from one that has said "no", and a bool defaulting
	// nil to false would read every never-asked environment as safe to
	// move -- which is exactly the environment CheckMoves exists to catch.
	// Check refuses the nil case; both true and false are answers it
	// accepts.
	Stateful *bool `json:"stateful"`
}

// Placement is where one environment runs. Exactly one of Cluster or Host
// is set, never both and never neither -- a placement naming both is not
// "extra information", it is two contradictory claims about where a
// workload runs, and nothing downstream can tell which one is true.
type Placement struct {
	Cluster   *string `json:"cluster"`
	Namespace *string `json:"namespace"`
	Host      *string `json:"host"`
}

// VaultRef names where an environment's secrets live in Vault. It never
// carries a secret value itself, only the mount and prefix a value is
// fetched from.
type VaultRef struct {
	Mount  string `json:"mount"`
	Prefix string `json:"prefix"`
}
