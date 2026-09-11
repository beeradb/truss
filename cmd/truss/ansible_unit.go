package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/beeradb/truss/internal/ansible"
	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/inventory"
	"github.com/beeradb/truss/internal/repo"
)

// ansibleRunner is the subset of ansible.Runner the pass uses, as a local
// interface for the same reason renderRunner and tofuRunner are: a test
// drives the whole configuration path -- gate, check-mode pre-run, apply,
// convergence check -- without an ansible-playbook binary or a machine to
// point it at.
type ansibleRunner interface {
	Check(ctx context.Context, playDir string, targets []ansible.Target) (ansible.Result, error)
	Apply(ctx context.Context, playDir string, targets []ansible.Target) (ansible.Result, error)
}

// ansibleFactory builds a runner for one pass, mirroring tofuFactory and
// renderFactory.
type ansibleFactory func(env []string) ansibleRunner

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
// each one cross-checked against live evidence from whichever provider
// vouches for it (see hostEvidence), or it does not run.
func runAnsibleUnits(ctx context.Context, d applyDeps, r ansibleRunner, headSHA string, plays []string) string {
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

	// ⚠️ WHAT WILL RUN IS SETTLED BEFORE ANY EVIDENCE IS GATHERED, AND THE
	// ORDER IS NOT COSMETIC. A provider that has to touch a machine to
	// observe it must touch only the machines this commit is about, so the
	// pass has to know which plays are actually going to run -- a retired
	// play, or one whose every host is frozen, names hosts that nobody is
	// about to configure and that nothing should be dialling on their
	// behalf.
	plans := ansiblePlans(d, snap, headSHA, plays)

	ev, reason := gatherEvidence(ctx, d, evidenceProviders(d, snap), snap, plans, headSHA)
	if reason != "" {
		return reason
	}

	for _, plan := range plans {
		targets := gates.AnsibleTargets{
			Play:     plan.play,
			Declared: plan.declared,
			// ⚠️ EVERY PLAY IS HANDED THE WHOLE PASS'S Undeclared, WHICH
			// IS WHAT MAKES CheckAnsibleTargets' "refuse every play, not
			// only this one" true. A refusal returns from this function and
			// fails the pass, so no later play runs -- and because the same
			// evidence goes to the first play as to the last, the refusal
			// cannot depend on which play happens to sort first.
			Unknown:     ev.Undeclared,
			Unreachable: intersect(ev.Unreachable, plan.declared),
		}
		if problems := gates.CheckAnsibleTargets(targets); len(problems) > 0 {
			return strings.Join(problems, "; ")
		}

		playDir := d.Cfg.Workdir + "/" + plan.play

		// ⚠️ BUILT FROM THE SAME NAMES THE GATE JUST VOUCHED FOR, never
		// from a second walk of the snapshot. plan.declared is what the
		// evidence was gathered about; a target list assembled any other
		// way could name a machine no provider was asked about, which is
		// the one thing the gate above exists to prevent.
		dial := ansibleTargets(snap, plan.declared)

		// ⚠️ CHECK MODE FIRST, AND ITS FAILURE REFUSES BEFORE ANYTHING
		// CHANGES. This is the closest thing a play has to a plan: an error
		// here -- an unreachable host, a task that cannot evaluate, a
		// missing variable -- is found while every machine is still
		// untouched, rather than on host four of six.
		before, err := r.Check(ctx, playDir, dial)
		if err != nil {
			return fmt.Sprintf("check mode refused %s at %s before anything ran: %v", plan.play, headSHA, err)
		}
		for _, h := range sortedHosts(before.ChangedByHost) {
			d.logf("ansible: %s would change %d task(s) on %s", plan.play, before.ChangedByHost[h], h)
		}

		if _, err := r.Apply(ctx, playDir, dial); err != nil {
			return fmt.Sprintf("could not run %s at %s: %v", plan.play, headSHA, err)
		}

		// ⚠️ THE CONVERGENCE CHECK NAMES, IT DOES NOT REFUSE. A host still
		// reporting work immediately after a successful run means a
		// non-idempotent task, which is a defect in the play -- but the run
		// already succeeded and the machine is already configured, so
		// failing the pass here would wedge the queue behind a change that
		// worked. This is the drift posture the whole project keeps: name,
		// never reconcile, and never punish the commit for what it revealed.
		after, err := r.Check(ctx, playDir, dial)
		if err != nil {
			d.logf("ansible: could not re-check %s at %s after applying it, so its convergence is unknown: %v", plan.play, headSHA, err)
			continue
		}
		for _, h := range sortedHosts(after.ChangedByHost) {
			if after.ChangedByHost[h] > 0 {
				d.logf("ansible: %s still reports %d changed task(s) on %s straight after a successful run -- a task in it is not idempotent", plan.play, after.ChangedByHost[h], h)
			}
		}
	}
	return ""
}

// playPlan is one play and the hosts it will be run against, settled before
// any evidence is gathered.
type playPlan struct {
	play string
	// declared is the play's hosts, minus the frozen and the
	// decommissioned. It may be EMPTY, and such a plan is deliberately
	// still in the list: an empty declared set is a refusal
	// (CheckAnsibleTargets), not something to quietly skip.
	declared []string
}

// ansibleTargets turns host NAMES into everything ansible needs to reach
// each machine, reading each one's own inventory record.
//
// ⚠️ A NAME WITH NO RECORD YIELDS A TARGET WITH NO ADDRESS, WHICH IS
// CORRECT AND NOT A HOLE. Only playHosts produces these names, and it
// produces them BY iterating the snapshot, so a missing record is
// unreachable here; if a future caller manages it anyway, a nameless
// address means ansible connects to the name -- the same behaviour a
// tailnet host gets, and a connection failure naming the host, rather than
// a silent omission from the inventory that would read as "configured".
func ansibleTargets(s inventory.Snapshot, names []string) []ansible.Target {
	out := make([]ansible.Target, 0, len(names))
	for _, n := range names {
		t := ansible.Target{Name: n}
		if h, ok := s.Hosts[n]; ok && h.Access != nil {
			// Address only for a host reached AT one. A tailnet host's
			// address is its name, which the renderer writes by writing
			// no ansible_host at all -- see ansible.Target.Address.
			if h.Access.Via == inventory.AccessAddress {
				t.Address = h.Access.Address
			}
			t.User = h.Access.User
		}
		// ⚠️ GROUPS ARE READ OFF THE RECORD, NEVER AUTHORED SEPARATELY.
		// A group list maintained beside the inventory is a second copy of
		// "which machines are dev workstations" and would be the stale one.
		// role is always present; cluster is optional and nil for a machine
		// that is in none.
		if h, ok := s.Hosts[n]; ok {
			if h.Role != "" {
				t.Groups = append(t.Groups, h.Role)
			}
			if h.Cluster != nil && *h.Cluster != "" {
				t.Groups = append(t.Groups, *h.Cluster)
			}
		}
		out = append(out, t)
	}
	return out
}

// playNames is the plays of a plan list, for a message that has to name
// what it is refusing.
func playNames(plans []playPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.play)
	}
	return out
}

// ansiblePlans decides which plays will run and against which hosts.
//
// ⚠️ IT REFUSES NOTHING, AND THE SPLIT IS THE POINT. Everything it drops --
// a retired play, a play whose every host is frozen -- is dropped because
// somebody deliberately arranged for it to run nothing, which is not a
// failure. Every actual refusal is downstream, where the evidence is, so
// there is exactly one place a play can be stopped and it is the target
// gate.
func ansiblePlans(d applyDeps, snap inventory.Snapshot, headSHA string, plays []string) []playPlan {
	var plans []playPlan
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
		plans = append(plans, playPlan{play: play, declared: declared})
	}
	return plans
}

// managedHostNames is every host git says is managed, WHICHEVER provider
// vouches for each -- the fleet a discovering provider measures what it
// sees against, and the reason a machine reached at a stated address is not
// an intruder just because it also turns up on the tailnet.
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
	env := []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
	if d.CollectionsPath != "" {
		env = append(env, "ANSIBLE_COLLECTIONS_PATH="+d.CollectionsPath)
	}
	return env
}

// defaultAnsibleBin is what a deployment gets when it does not set
// ANSIBLE_BIN, in the optional-with-default shape kustomizeBin uses -- and,
// like that one, deliberately not a config.Config field, because
// config_test.go pins the exact count of those and this knob is not the
// applier's to grow.
const defaultAnsibleBin = "/usr/local/bin/ansible-playbook"

// ansibleCollectionsPath is where ansible-playbook should look for
// collections, passed through from the process environment.
//
// ⚠️ AN IMAGE CANNOT SET THIS WITH `ENV` AND HAVE IT REACH THE PLAY, AND
// truss's OWN SAFETY PROPERTY IS WHY. ansible.Runner builds the child's
// environment EXPLICITLY -- "Nil means an empty environment, never an
// inherited one", because inheriting would hand a play running as root on
// somebody else's machine every cloud credential this process holds. So a
// Dockerfile line like
//
//	ENV ANSIBLE_COLLECTIONS_PATH=/opt/ansible/collections
//
// is silently discarded, and a collection installed anywhere but ansible's
// own default search path is invisible. Measured 2026-09-11: a pass failed
// with "couldn't resolve module/action 'ansible.posix.mount'" while
// ansible.posix:2.2.2 was pinned in `ansible-collections` and installed in
// the image. The collection was there; nothing told ansible where.
//
// ⚠️ NAMED PASSTHROUGH, NOT INHERITANCE. This is one variable, read by
// name, exactly as ANSIBLE_BIN already is -- it does not weaken the rule
// above, which is about the WHOLE environment crossing into a play. Unset
// means unset: ansible's own defaults apply, which is correct for a
// deployment that installs collections where ansible already looks.
func ansibleCollectionsPath(getenv func(string) string) string {
	return getenv("ANSIBLE_COLLECTIONS_PATH")
}

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
