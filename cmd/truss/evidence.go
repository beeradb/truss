package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/beeradb/truss/internal/inventory"
)

// hostEvidence is one source of live fact about the machines a play would
// configure. It is the observation half of the ansible target gate: the
// committed inventory says which hosts a play DECLARES, and a provider says
// what is actually out there right now.
//
// ⚠️ IT IS AN INTERFACE BECAUSE TAILSCALE IS ONE PROVIDER, NOT THE
// DEFINITION OF EVIDENCE. The first version of this gate read
// `d.Tailnet == nil` and refused, which meant a truss user who does not run
// Tailscale could not run an ansible unit at all -- this deployment's
// process compiled into a general engine, a heavier coupling than the
// identifying values the engine extraction already lifts out. It also made
// first contact impossible in the only order that works: load a host at an
// external address, run the play, join the tailnet, lock SSH down from
// outside, then point the record at the internal name. A host that has
// never joined has no device record, so it could never be a target, so the
// play that would have joined it could never run.
//
// ⚠️ WHAT DOES NOT MOVE IS THE INVARIANT. A play is gated by its target set
// and nothing else, so a deployment where NO provider can vouch for a host
// still refuses -- see evidenceRefusal. Making evidence pluggable is not
// making it optional, and internal/gates.CheckAnsibleTargets, which does
// the actual refusing, is untouched by any of this: it already took
// Declared, Unknown and Unreachable and knew nothing about where they came
// from, which is exactly why the seam belongs here and not there.
//
// It observes and it does not act -- the same rule internal/tailnet's
// package doc states for its own side, and for the same reason: git is the
// source of truth for what is managed, and no provider may add a host to
// what the applier will touch.
type hostEvidence interface {
	// Name is what refusals and the pass log call this provider. It is the
	// same string an inventory record's access.via names.
	Name() string

	// Discovers reports whether this provider can find a machine that
	// CLAIMS to be managed and that no inventory record names.
	//
	// ⚠️ IT IS A METHOD ON THE INTERFACE RATHER THAN A CONVENTION ABOUT AN
	// EMPTY SLICE, AND THAT IS THE WHOLE POINT OF THIS DESIGN. "I looked
	// and found none" and "nothing here can look" are different
	// guarantees, and an empty Undeclared cannot tell them apart. A
	// provider author cannot forget to answer this, because the compiler
	// will not let them: an unanswered capability that defaults to "yes"
	// is a gate reporting a clean fleet it never examined.
	Discovers() bool

	// Observe reports what this provider can see right now, or an error if
	// it could not look at all -- which is never the same as seeing
	// nothing (internal/secrets.Store.List records the identical rule for
	// an unreadable store versus an empty one).
	//
	// fleet is every host name the committed inventory declares as
	// managed, WHICHEVER provider vouches for each. ⚠️ A DISCOVERING
	// PROVIDER MUST MEASURE AGAINST ALL OF THEM AND NOT ONLY ITS OWN
	// SHARE. A machine vouched for by another provider is still declared,
	// and a tailnet compared against only the tailnet-vouched names would
	// report every address-reached host on it as an intruder -- refusing
	// every play in the pass over a host somebody did write down.
	//
	// targets is the subset of fleet this provider is responsible for AND
	// whose reachability this pass actually needs. A provider that must
	// touch a machine to observe it touches only these: probing a host no
	// play in this commit configures is reach nothing asked for, and its
	// answer would be discarded unread.
	Observe(ctx context.Context, fleet, targets []string) (observation, error)
}

// observation is one provider's answer.
type observation struct {
	// Unreachable is every name in targets this provider could not
	// observe. ⚠️ Absent is not "fine": the caller refuses on it, never
	// skips (internal/tailnet.Findings.Unreachable's own doc).
	Unreachable []string
	// Undeclared is every machine this provider found claiming to be
	// managed with no inventory record. Always empty when Discovers is
	// false -- and a provider that returns names anyway is refused rather
	// than quietly believed, see gatherEvidence.
	Undeclared []string
}

// passEvidence is every provider's answer for one pass, merged.
type passEvidence struct {
	// Unreachable is the union across providers, narrowed to one play's
	// own hosts by the caller.
	Unreachable []string
	// Undeclared is the union across the providers that can discover.
	Undeclared []string
	// Blind names every provider in this pass that CANNOT discover an
	// undeclared machine.
	//
	// ⚠️ IT IS NOT THE SAME FACT AS AN EMPTY Undeclared, AND KEEPING THEM
	// APART IS WHY THIS FIELD EXISTS. A pass that searched the whole
	// tailnet and found no intruder, and a pass that had no way to search
	// at all, both produce zero names -- and they are different promises
	// to the person reading the log. A non-empty Blind means the pass's
	// answer to "is there a machine here nobody declared?" is partial, and
	// the applier says which provider made it partial rather than letting
	// silence imply a clean fleet.
	//
	// It is NOT a refusal. A declared-address deployment is a supported
	// way to run truss, not a broken tailnet deployment, and refusing it
	// would make the provider unusable while pretending to be careful.
	// What it gets is disclosure, on every pass, in the same posture a
	// frozen host gets: named every time, never silently skipped.
	Blind []string
}

// accessOf answers which provider vouches for one host, and at what
// address. See inventory.Host.Access for why a nil access block resolves to
// tailscale rather than being refused, and why that cannot fail open.
func accessOf(h inventory.Host) (via, address string) {
	if h.Access == nil {
		return inventory.AccessTailscale, ""
	}
	return h.Access.Via, h.Access.Address
}

// evidenceSet is the providers this pass has available, keyed by the
// access.via value an inventory record names. A provider needing a
// credential that is not mounted is simply absent, which is what
// evidenceRefusal turns into a refusal naming the host that needed it.
type evidenceSet map[string]hostEvidence

// gatherEvidence asks every provider this pass needs what it can see, and
// merges the answers. It returns the merged evidence, or the reason to fail
// the pass.
//
// ⚠️ IT RUNS ONCE FOR THE WHOLE PASS, BEFORE ANY PLAY, WHICH IS WHAT MAKES
// "one unknown device refuses every play" TRUE BY CONSTRUCTION rather than
// by a caller's discipline. Every play is then handed the same Undeclared,
// so the refusal cannot depend on which play happens to sort first --
// gates.CheckAnsibleTargets' own doc demands this of its caller and cannot
// enforce it, because it sees one play at a time.
func gatherEvidence(ctx context.Context, d applyDeps, providers evidenceSet, snap inventory.Snapshot, plans []playPlan, headSHA string) (passEvidence, string) {
	fleet := managedHostNames(snap)

	// targets is each provider's share of the hosts the plays about to run
	// actually name. A host whose provider is not available refuses here,
	// before anything is observed and long before anything is configured.
	targets := map[string][]string{}
	for _, p := range plans {
		for _, host := range p.declared {
			via, _ := accessOf(snap.Hosts[host])
			if _, ok := providers[via]; !ok {
				return passEvidence{}, evidenceRefusal(p.play, headSHA, host, via)
			}
			targets[via] = append(targets[via], host)
		}
	}

	var ev passEvidence
	for _, name := range sortedProviders(providers) {
		provider := providers[name]
		// ⚠️ A DISCOVERING PROVIDER IS ASKED WHETHER OR NOT A HOST NAMES
		// IT, AND A BLIND ONE IS NOT. An undeclared machine hides
		// precisely by not being declared, so asking only the providers
		// some record points at would let a pass be blinded by the very
		// records it is checking -- move every host to another provider
		// and the intruder search stops happening. The converse keeps the
		// disclosure honest: a deployment using no address-reached hosts
		// is not told about a limitation it does not have.
		if !provider.Discovers() && len(targets[name]) == 0 {
			continue
		}

		obs, err := provider.Observe(ctx, fleet, targets[name])
		if err != nil {
			return passEvidence{}, fmt.Sprintf("could not read host evidence from %s before running %s at %s: %v", name, strings.Join(playNames(plans), ", "), headSHA, err)
		}

		ev.Unreachable = append(ev.Unreachable, obs.Unreachable...)
		if provider.Discovers() {
			ev.Undeclared = append(ev.Undeclared, obs.Undeclared...)
		} else {
			// ⚠️ NAMES FROM A PROVIDER THAT SAYS IT CANNOT DISCOVER ARE A
			// REFUSAL, NOT SOMETHING TO DROP. Merging them would trust a
			// provider that contradicts its own declared capability;
			// dropping them silently would hide an intruder it somehow
			// did find. Either way the pass would be acting on a provider
			// that does not know what it is, so it stops.
			if len(obs.Undeclared) > 0 {
				return passEvidence{}, fmt.Sprintf("refusing to run %s at %s: the %s host-evidence provider reports Discovers() false but returned undeclared machine(s) (%s) -- a provider that contradicts its own capability cannot be believed in either direction", strings.Join(playNames(plans), ", "), headSHA, name, strings.Join(obs.Undeclared, ", "))
			}
			ev.Blind = append(ev.Blind, name)
		}

		d.logf("ansible: %s vouched for %d of the %d host(s) it was asked about at %s", name, len(targets[name])-len(obs.Unreachable), len(targets[name]), headSHA)
	}

	ev.Unreachable = sortedUnique(ev.Unreachable)
	ev.Undeclared = sortedUnique(ev.Undeclared)
	ev.Blind = sortedUnique(ev.Blind)

	// Said every pass, never once at adoption: an operator must be able to
	// tell WHICH guarantee this pass gave them, and a limitation stated
	// only in a design document is one nobody is reading at three in the
	// morning. Cheap, one line, and only when it is true.
	if len(ev.Blind) > 0 {
		d.logf("ansible: %s cannot discover a machine claiming to be managed that no inventory record names, so nothing in this pass can catch one reached that way -- a strictly weaker guarantee than the tailnet's, and a property of how those hosts are reached rather than a gap to be closed here", strings.Join(ev.Blind, ", "))
	}
	return ev, ""
}

// evidenceRefusal explains why nothing in this pass can vouch for one host.
//
// ⚠️ THIS IS THE REFUSAL THAT MUST NOT BE WEAKENED BY MAKING EVIDENCE
// PLUGGABLE. KindAnsible has no plan digest -- CI cannot reach the hosts a
// play would run against -- so the target set is the whole gate, and a
// target set nothing can vouch for is a gate that passes. Running anyway
// would be the target gate failing open, which internal/gates exists to
// forbid. It costs a deployment nothing until it commits its first play,
// which is also the commit that makes the refusal correct.
func evidenceRefusal(play, headSHA, host, via string) string {
	switch {
	case !containsString(inventory.AccessVias(), via):
		return fmt.Sprintf("refusing to run %s at %s: %s declares access.via %q, which this build does not understand -- it knows %s; a host whose access nobody can read is a host nothing can vouch for, and a play is gated by its target set and nothing else",
			play, headSHA, host, via, strings.Join(quotedVias(), " or "))
	case via == inventory.AccessTailscale:
		return fmt.Sprintf("refusing to run %s at %s: %s is reached over the tailnet, but no tailscale credential is mounted, so the applier has no evidence about which hosts exist -- a play is gated by its target set and nothing else, so without that evidence there is no gate",
			play, headSHA, host)
	default:
		return fmt.Sprintf("refusing to run %s at %s: nothing in this pass can vouch for %s, which declares access.via %q -- a play is gated by its target set and nothing else, so without evidence there is no gate",
			play, headSHA, host, via)
	}
}

func quotedVias() []string {
	vias := inventory.AccessVias()
	out := make([]string, len(vias))
	for i, v := range vias {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

// sortedProviders returns the set's keys in a fixed order, so two passes
// over the same facts observe in the same order and log the same way.
func sortedProviders(set evidenceSet) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedUnique sorts and de-duplicates, so a name two providers both
// reported appears once. Nil for empty, matching this repository's
// convention that "found nothing" and "found zero of something" are the
// same value (internal/tailnet.sortedKeys says the same).
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
