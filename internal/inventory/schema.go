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
	// Access says HOW the applier reaches this machine, which decides
	// WHICH provider is allowed to vouch that it exists before a play is
	// run against it. See Access.
	//
	// ⚠️ NIL IS TOLERATED AND MEANS "tailscale", WHICH IS THE ONE PLACE
	// THIS PACKAGE READS AN UNSTATED FIELD AS AN ANSWER -- and it is a
	// deliberate exception to the rule HasHAVault and Stateful state, not
	// a lapse from it. Those two refuse nil because nobody has ever
	// answered them and the answer changes what happens. Every host record
	// written before this field existed was, necessarily, reached over the
	// tailnet: it was the only thing the applier could reach a machine
	// through, and the only evidence it would accept. So nil is not an
	// unanswered question here, it is a question that had exactly one
	// possible answer at the time the record was written, and reading it
	// as that answer is the only default that cannot silently change what
	// an existing commit does.
	//
	// It does not fail open: a host that resolves to "tailscale" in a
	// deployment with no tailscale credential mounted is still refused, by
	// the same rule as one that says so out loud. And the applier names
	// every such host on every pass, so the tolerance is visible rather
	// than assumed.
	Access *Access `json:"access"`
}

// The values Access.Via may take. Each names ONE provider of host
// evidence; the applier maps them onto the thing that does the observing.
//
// ⚠️ THEY ARE A CLOSED SET AND AN UNRECOGNISED ONE IS REFUSED, not ignored.
// A record whose access nobody can read is a record nothing can vouch for,
// and the failure of accepting it quietly is a host that gets configured on
// the strength of no evidence at all.
const (
	// AccessTailscale: the machine is reached over the tailnet, and
	// Tailscale's own device list is the evidence it exists.
	AccessTailscale = "tailscale"
	// AccessAddress: the machine is reached at an address stated in the
	// record, and a live probe of that address is the evidence it exists.
	AccessAddress = "address"
)

// AccessVias is every value Access.Via may take, sorted, for a refusal to
// name when it has just refused one that is not among them.
func AccessVias() []string { return []string{AccessAddress, AccessTailscale} }

// Access says how truss reaches one machine.
//
// ⚠️ IT IS NOT A BOOTSTRAP PHASE AND MUST NOT BE DESIGNED AS ONE. For this
// author's own deployment a declared address is transitional -- load a host
// on its external address, run the play, join the tailnet, lock SSH down
// from outside, then point the record at the internal name. For somebody
// else it is the whole and permanent story: a fleet reached by SSH over
// stated addresses is a real way to run machines, and truss refusing to
// configure one would be this deployment's process compiled into a general
// engine. Anything shaped as "temporary until the tailnet is up" rebuilds
// that coupling more slowly.
//
// ⚠️ AND THE TWO ARE NOT EQUALLY SAFE, WHICH IS A FACT ABOUT THE WORLD AND
// NOT A GAP IN THIS CODE. Tailscale can be asked "which machines claim to
// be managed?" and answer with machines nobody declared -- an intruder, or
// a host somebody forgot -- which is the single most valuable thing the
// ansible target gate does. An address can only be asked about the hosts
// the inventory already names, so it can never discover an undeclared
// machine, and no amount of probing will make it able to. A deployment
// reaching its hosts this way gets a strictly weaker guarantee; the applier
// says so on every pass rather than letting an empty answer read as "none
// found".
type Access struct {
	// Via names the provider. One of AccessVias().
	Via string `json:"via"`
	// Address is the machine's address as "<host>" or "<host>:<port>",
	// required when Via is AccessAddress and refused otherwise -- see
	// checkHost. It is where the applier DIALS, never where it looks the
	// machine up: a name here that only the tailnet resolves is a record
	// that says "address" and means "tailscale".
	Address string `json:"address"`
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
