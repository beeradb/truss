package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/ansible"
	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/inventory"
	"github.com/beeradb/truss/internal/repo"
	"github.com/beeradb/truss/internal/tailnet"
)

// ansibleRunner is the subset of ansible.Runner the pass uses, as a local
// interface for the same reason renderRunner and tofuRunner are: a test
// drives the whole configuration path -- gate, check-mode pre-run, apply,
// convergence check -- without an ansible-playbook binary or a machine to
// point it at.
type ansibleRunner interface {
	Check(ctx context.Context, playDir string, hosts []string) (ansible.Result, error)
	Apply(ctx context.Context, playDir string, hosts []string) (ansible.Result, error)
}

// ansibleFactory builds a runner for one pass, mirroring tofuFactory and
// renderFactory.
type ansibleFactory func(env []string) ansibleRunner

// tailnetLister is the live observation half of the target gate. It is an
// interface rather than a *tailnet.Client so a test can supply devices, and
// it lists only -- there is deliberately no method here that changes
// anything on the tailnet, because git is the source of truth for what is
// managed and this side is evidence (internal/tailnet's package doc).
type tailnetLister interface {
	Devices(ctx context.Context) ([]tailnet.Device, error)
}

// managedTag is the ACL tag that means "the applier may configure this
// machine". It is COMPILED IN, not configured, for the reason roots and
// unit kinds are: which tag causes configuration to execute on a machine is
// an engine value, and a deployment that could rename it could point the
// applier at a tag somebody else is allowed to apply. It matches the tag the
// plan grants `tag:applier` SSH to.
const managedTag = "tag:managed"

// tailnetStaleAfter is how long since a device was last seen before the
// applier stops believing a declared host is reachable.
//
// ⚠️ A CONNECTED DEVICE IS NEVER STALE BY THIS CLOCK. Tailscale omits
// `lastSeen` entirely for a device that is connected to the control server
// right now, and tailnet.Devices folds that case into "seen at the moment of
// this call" (tailnet.Device.LastSeen's own doc), so this bound only ever
// judges a device that has actually gone away.
//
// One hour rather than a tighter figure because the failure this prevents is
// not subtle: a play that starts against a host it cannot reach dies partway
// through and leaves that machine half configured, which is worse than the
// pass refusing before it touched anything. An hour is long enough to
// survive clock skew and a short outage, short enough that "in the inventory
// and gone" is still a fact about now.
const tailnetStaleAfter = time.Hour

// ansibleUnitsFor derives the plays a commit touches: the ansible half of
// the same repo.TouchedUnits call whose credentials/tofu half tofuUnitsFor
// reads and whose render half renderUnitsFor reads.
//
// ⚠️ treeUnits IS THE PLAYS OF THE TREE, NOT EVERY UNIT -- fed from
// gitDriver.TreeAnsibleUnits. The three calls partition TouchedUnits' output
// by construction rather than by filtering a combined listing, so no unit
// can be claimed by two kinds and none can be dropped by both.
func ansibleUnitsFor(changedFiles, treeUnits []string) []string {
	var out []string
	for _, u := range repo.TouchedUnits(changedFiles, treeUnits) {
		if u.Kind == repo.KindAnsible {
			out = append(out, u.Path)
		}
	}
	return out
}

// runAnsibleUnits configures machines for one commit: it proves the target
// set before anything runs, runs each play in check mode, applies it, then
// runs check mode again and names any host that still reports work.
//
// It returns the reason to fail the pass, or empty.
//
// ⚠️ THE TARGET SET IS THE WHOLE GATE, BECAUSE THERE IS NO DIGEST HERE.
// KindAnsible has no plan digest and will not be given one: CI cannot reach
// the hosts, so anything CI could file would be a function of the commit
// alone, and the commit is already pinned by the merge-provenance gate --
// a check that cannot fail. What review means for a play is the diff, the
// precedent docs/credentials.md sets for the credentials root, plus this:
// the play runs against exactly the hosts the committed inventory hands it,
// cross-checked against the live tailnet, or it does not run.
func runAnsibleUnits(ctx context.Context, d applyDeps, r ansibleRunner, headSHA string, plays []string) string {
	// ⚠️ NO TAILNET MEANS NO PLAY, NOT A PLAY WITHOUT THE CHECK. A
	// deployment that has not mounted the tailscale credential simply has no
	// evidence about its hosts, and running a play anyway would be the
	// target gate failing open -- the precise thing internal/gates exists to
	// forbid. This costs a deployment nothing until it commits its first
	// play, which is also the commit that makes the refusal correct.
	if d.Tailnet == nil {
		return fmt.Sprintf("refusing to run %s at %s: no tailscale credential is mounted, so the applier has no evidence about which hosts exist -- a play is gated by its target set and nothing else, so without that evidence there is no gate", strings.Join(plays, ", "), headSHA)
	}

	headFS, err := d.Git.TreeFS(ctx, headSHA)
	if err != nil {
		return fmt.Sprintf("could not read the inventory tree at %s: %v", headSHA, err)
	}
	// ⚠️ AN ABSENT inventory/ IS A REFUSAL HERE, WHERE checkInventoryAtCommit
	// TREATS IT AS "THIS DEPLOYMENT HAS NOT ADOPTED THE INVENTORY" AND
	// RETURNS CLEAN. The difference is not inconsistency: that function runs
	// for every commit, including in trees that have no inventory and never
	// will, so skipping is the only honest answer. This function runs only
	// when a play exists, and a play with no inventory has no declared
	// hosts -- which is the one input that must never be read as "run
	// against everything".
	if _, err := fs.Stat(headFS, "inventory"); err != nil {
		return fmt.Sprintf("refusing to run %s at %s: the tree has a play but no inventory/ directory, so no host is declared for it", strings.Join(plays, ", "), headSHA)
	}
	snap, problems := inventory.Load(headFS)
	if len(problems) > 0 {
		// Unreachable in the pass, where checkInventoryAtCommit already
		// refused this commit before any unit was derived. Kept because this
		// function is called directly by tests and could be by a future
		// caller, and reading hosts out of an inventory that did not load is
		// how a play acquires a target set nobody wrote.
		return fmt.Sprintf("refusing to run %s at %s: the inventory does not load: %s", strings.Join(plays, ", "), headSHA, strings.Join(problems, "; "))
	}

	devices, err := d.Tailnet.Devices(ctx)
	if err != nil {
		return fmt.Sprintf("could not list the tailnet before running %s at %s: %v", strings.Join(plays, ", "), headSHA, err)
	}
	findings := tailnet.Reconcile(devices, managedHostNames(snap), managedTag, d.now(), tailnetStaleAfter)

	for _, play := range plays {
		// ⚠️ A PLAY THAT NO LONGER EXISTS IS A RETIREMENT, NOT A REFUSAL AND
		// NOT A PRUNE. applyOneRoot refuses an absent root because its state
		// may hold live resources; renderOneUnit treats an absent unit as a
		// prune because a reconciler removes what it applied. Neither fits a
		// machine: deleting a play does not un-configure anything, the host
		// keeps exactly the configuration it already has, and saying
		// otherwise -- in either direction -- would be a lie about the state
		// of somebody's machine.
		if !d.Git.HasDir(play) {
			d.logf("ansible: %s is gone at %s; its hosts keep the configuration they already have", play, headSHA)
			continue
		}

		declared, frozen := playHosts(snap, play)
		for _, h := range frozen {
			// Named every pass, never silently skipped: frozen is an
			// operator's deliberate "do not touch", and an operator who
			// forgot they set it must be told on every pass rather than
			// discovering it when a change fails to take effect.
			d.logf("ansible: %s is frozen; %s will not configure it", h, play)
		}
		// A play whose every host is frozen is skipped, not refused. The
		// gate's empty-Declared refusal is about BROKEN WIRING -- a play no
		// host names -- and freezing is the opposite of that: somebody
		// declared these hosts and then said not to touch them. Refusing
		// here would make freezing a host wedge the queue.
		if len(declared) == 0 && len(frozen) > 0 {
			d.logf("ansible: every host of %s is frozen; nothing to configure at %s", play, headSHA)
			continue
		}

		targets := gates.AnsibleTargets{
			Play:     play,
			Declared: declared,
			// ⚠️ EVERY PLAY IS HANDED THE WHOLE PASS'S UnknownTagged, WHICH
			// IS WHAT MAKES CheckAnsibleTargets' "refuse every play, not
			// only this one" true. A refusal returns from this function and
			// fails the pass, so no later play runs -- and because the same
			// findings go to the first play as to the last, the refusal
			// cannot depend on which play happens to sort first.
			Unknown:     findings.UnknownTagged,
			Unreachable: intersect(findings.Unreachable, declared),
		}
		if problems := gates.CheckAnsibleTargets(targets); len(problems) > 0 {
			return strings.Join(problems, "; ")
		}

		playDir := d.Cfg.Workdir + "/" + play

		// ⚠️ CHECK MODE FIRST, AND ITS FAILURE REFUSES BEFORE ANYTHING
		// CHANGES. This is the closest thing a play has to a plan: an error
		// here -- an unreachable host, a task that cannot evaluate, a
		// missing variable -- is found while every machine is still
		// untouched, rather than on host four of six.
		before, err := r.Check(ctx, playDir, declared)
		if err != nil {
			return fmt.Sprintf("check mode refused %s at %s before anything ran: %v", play, headSHA, err)
		}
		for _, h := range sortedHosts(before.ChangedByHost) {
			d.logf("ansible: %s would change %d task(s) on %s", play, before.ChangedByHost[h], h)
		}

		if _, err := r.Apply(ctx, playDir, declared); err != nil {
			return fmt.Sprintf("could not run %s at %s: %v", play, headSHA, err)
		}

		// ⚠️ THE CONVERGENCE CHECK NAMES, IT DOES NOT REFUSE. A host still
		// reporting work immediately after a successful run means a
		// non-idempotent task, which is a defect in the play -- but the run
		// already succeeded and the machine is already configured, so
		// failing the pass here would wedge the queue behind a change that
		// worked. This is the drift posture the whole project keeps: name,
		// never reconcile, and never punish the commit for what it revealed.
		after, err := r.Check(ctx, playDir, declared)
		if err != nil {
			d.logf("ansible: could not re-check %s at %s after applying it, so its convergence is unknown: %v", play, headSHA, err)
			continue
		}
		for _, h := range sortedHosts(after.ChangedByHost) {
			if after.ChangedByHost[h] > 0 {
				d.logf("ansible: %s still reports %d changed task(s) on %s straight after a successful run -- a task in it is not idempotent", play, after.ChangedByHost[h], h)
			}
		}
	}
	return ""
}

// managedHostNames is every host git says is managed, which is what
// tailnet.Reconcile compares the live device list against.
//
// ⚠️ A DECOMMISSIONED HOST IS NOT MANAGED, AND A FROZEN ONE IS. Frozen means
// "do not configure this now", so a frozen host that has vanished from the
// tailnet is still a fact somebody should hear about; decommissioned means
// the machine is gone on purpose, and reporting it as unreachable for ever
// would train an operator to ignore the one list that must stay short.
func managedHostNames(s inventory.Snapshot) []string {
	var out []string
	for name, h := range s.Hosts {
		if h.Decommissioned {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// playHosts splits the hosts one play configures into the ones it will run
// against and the ones it will not because they are frozen. Both are sorted,
// so the --limit a play is given is a function of the tree and never of map
// iteration order -- two passes over one commit must build the same argv.
func playHosts(s inventory.Snapshot, play string) (declared, frozen []string) {
	for name, h := range s.Hosts {
		if h.Decommissioned || h.Config == nil || *h.Config != play {
			continue
		}
		if h.Frozen {
			frozen = append(frozen, name)
			continue
		}
		declared = append(declared, name)
	}
	sort.Strings(declared)
	sort.Strings(frozen)
	return declared, frozen
}

// intersect returns the members of all that are in only, preserving all's
// order.
//
// ⚠️ IT NARROWS Unreachable TO ONE PLAY'S OWN HOSTS, WHICH IS THE OPPOSITE
// OF WHAT IS DONE WITH UnknownTagged, AND THE ASYMMETRY IS THE POINT. An
// undeclared machine wearing the managed tag is a fleet-wide fact -- nobody
// knows what it is, so no play should run. A declared host being offline is
// a fact about that host: it must refuse the play that configures it, and it
// must not stop an unrelated play configuring machines that are up.
func intersect(all, only []string) []string {
	set := make(map[string]bool, len(only))
	for _, s := range only {
		set[s] = true
	}
	var out []string
	for _, s := range all {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// sortedHosts returns m's keys in a fixed order, so a pass's log reads the
// same way twice over the same facts.
func sortedHosts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ansibleEnv is the whole environment a play gets: PATH so the binary and
// its interpreter can be found, HOME because ansible writes a control-path
// directory and a fact cache there.
//
// ⚠️ NO CREDENTIAL, AND NO SSH PRIVATE KEY ANYWHERE. The applier reaches a
// managed machine as its own tailnet identity, over Tailscale SSH, which the
// reviewed ACL grants from tag:applier to tag:managed -- so authorisation to
// configure a host is a commit somebody approved rather than a key somebody
// holds. Passing the pass's environment here instead would hand every task
// in an arbitrary play the applier's cloud credentials, on someone else's
// machine, which is the reach ansible.Runner.Env's own doc refuses.
func ansibleEnv(d applyDeps) []string {
	return []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
}

// defaultAnsibleBin is what a deployment gets when it does not set
// ANSIBLE_BIN, in the optional-with-default shape kustomizeBin uses -- and,
// like that one, deliberately not a config.Config field, because
// config_test.go pins the exact count of those and this knob is not the
// applier's to grow.
const defaultAnsibleBin = "/usr/local/bin/ansible-playbook"

func ansibleBin(getenv func(string) string) string {
	if bin := getenv("ANSIBLE_BIN"); bin != "" {
		return bin
	}
	return defaultAnsibleBin
}

// newAnsibleFactory builds the pass's ansible factory, the sibling of
// newRenderFactory.
func newAnsibleFactory(getenv func(string) string, stderr io.Writer) ansibleFactory {
	bin := ansibleBin(getenv)
	return func(env []string) ansibleRunner {
		return ansible.Runner{Bin: bin, Stderr: stderr, Env: env}
	}
}
