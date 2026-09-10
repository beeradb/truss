package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/beeradb/truss/internal/inventory"
	"github.com/beeradb/truss/internal/reach"
	"github.com/beeradb/truss/internal/tailnet"
)

// --- tailscale ----------------------------------------------------------

// tailnetLister is the live observation half of the tailscale provider. It
// is an interface rather than a *tailnet.Client so a test can supply
// devices, and it lists only -- there is deliberately no method here that
// changes anything on the tailnet, because git is the source of truth for
// what is managed and this side is evidence (internal/tailnet's package
// doc).
type tailnetLister interface {
	Devices(ctx context.Context) ([]tailnet.Device, error)
}

// managedTag is the ACL tag that means "the applier may configure this
// machine". It is COMPILED IN, not configured, for the reason roots and
// unit kinds are: which tag causes configuration to execute on a machine is
// an engine value, and a deployment that could rename it could point the
// applier at a tag somebody else is allowed to apply. It matches the tag the
// plan grants `tag:applier` SSH to.
//
// ⚠️ IT LIVES INSIDE THIS PROVIDER, WHICH IS WHERE THE REASONING ALWAYS
// POINTED. The argument above is entirely about Tailscale's ACL model --
// tags, who may apply one, what SSH grant they carry -- and none of it says
// anything to a deployment that reaches its machines at stated addresses.
// Held as an engine-wide constant it read as "this is what managed means";
// held here it reads as "this is what Tailscale's answer to that question
// is", which is what it always was. It is still not configurable, and
// deliberately so.
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
//
// ⚠️ IT IS A BOUND ON A RECORD, NOT ON A CONNECTION, which is why it is an
// hour where reach.DefaultTimeout is three seconds. Tailscale reports when
// it last heard from a machine; a probe makes the connection itself. The
// two are not comparable numbers and neither should be derived from the
// other.
const tailnetStaleAfter = time.Hour

// tailscaleEvidence vouches for machines from Tailscale's own device list.
//
// It is the provider that can DISCOVER: asked which machines claim to be
// managed, it answers with every device wearing the managed tag, including
// ones no inventory record names. That is the single most valuable thing
// the ansible target gate does, and no other provider here can do it.
type tailscaleEvidence struct {
	lister tailnetLister
	now    func() time.Time
}

func (tailscaleEvidence) Name() string { return inventory.AccessTailscale }

// Discovers is true, and it is the reason this provider is asked on every
// pass whether or not any host names it.
func (tailscaleEvidence) Discovers() bool { return true }

func (t tailscaleEvidence) Observe(ctx context.Context, fleet, targets []string) (observation, error) {
	devices, err := t.lister.Devices(ctx)
	if err != nil {
		return observation{}, err
	}
	findings := tailnet.Reconcile(devices, fleet, managedTag, t.now(), tailnetStaleAfter)

	// ⚠️ Unreachable IS NARROWED TO THIS PROVIDER'S OWN TARGETS, AND
	// UnknownTagged IS NOT. Reconcile is handed the WHOLE fleet because an
	// undeclared machine is only undeclared relative to every record there
	// is -- narrow that side and an address-reached host that also happens
	// to be on the tailnet reads as an intruder. But the other side of the
	// same call then reports every address-reached host that is NOT on the
	// tailnet as unreachable, which is not this provider's judgement to
	// make: those machines were never claimed to be there, and the probe
	// that does vouch for them has its own answer. Reconcile cannot make
	// this distinction itself -- it is pure, it knows nothing about
	// inventory records, and that is deliberate -- so the narrowing
	// belongs here.
	return observation{
		Unreachable: intersect(findings.Unreachable, targets),
		Undeclared:  findings.UnknownTagged,
	}, nil
}

// --- declared address ---------------------------------------------------

// addressEvidence vouches for a machine by dialling the address its own
// inventory record states.
//
// ⚠️ IT IS A FIRST-CLASS PROVIDER AND NOT BOOTSTRAP SCAFFOLDING. For a
// deployment that reaches its machines by SSH over stated addresses this is
// the permanent and complete story, not a phase to grow out of. It is also
// what makes FIRST CONTACT possible for a deployment that does end up on a
// tailnet: a host that has never joined has no device record, so under a
// tailscale-only gate it could never be a target, so the play that would
// have joined it could never run. The order that works is load the host at
// an external address, run the play, join, lock SSH down from outside, then
// point the record at the internal name -- and every step of that is an
// ordinary reviewed commit rather than somebody on a laptop.
//
// ⚠️ AND IT CANNOT DISCOVER. See Discovers.
type addressEvidence struct {
	// hosts is the committed inventory's host records, which is where an
	// address comes from. The provider never takes one from anywhere else:
	// an address the applier was told at runtime would be a target set
	// nobody reviewed.
	hosts  map[string]inventory.Host
	prober reach.Prober
}

func (addressEvidence) Name() string { return inventory.AccessAddress }

// Discovers is FALSE, and this is the honest statement of a real reduction
// in safety rather than an implementation gap.
//
// ⚠️ THERE IS NOTHING TO ASK. Tailscale holds a list of every machine on
// the tailnet and will hand over the ones wearing the managed tag whether
// or not we declared them, which is how an intruder or a forgotten host is
// found. An address provider has no such list and cannot acquire one: the
// only addresses it knows are the ones the inventory already names, so the
// set it can look at is by construction the set that is already declared.
// It could scan a network range instead, and that would be a different and
// much worse thing -- truss connecting to machines nobody wrote down, on
// the strength of a guess about where they might be.
//
// So a deployment reaching hosts this way gets a strictly weaker guarantee
// than one on a tailnet, and the applier says so on every pass
// (passEvidence.Blind). What it must NOT do is return an empty Undeclared
// and let that read as "looked, found none" -- that is the same claim by
// omission this project refuses everywhere else, and it would be the
// dangerous direction of the ansible target gate quietly turned off.
func (addressEvidence) Discovers() bool { return false }

// Observe dials each target's declared address.
//
// ⚠️ fleet IS IGNORED HERE, AND THE UNUSED PARAMETER IS THE ASYMMETRY MADE
// VISIBLE IN THE CODE ITSELF. fleet exists so a provider can measure what
// it sees against every host anybody declared; this provider sees nothing
// it was not pointed at, so there is nothing to measure. Deleting the
// parameter from the interface would make the two providers look
// interchangeable, which is the impression this whole design exists to
// prevent.
func (a addressEvidence) Observe(ctx context.Context, fleet, targets []string) (observation, error) {
	var unreachable []string
	for _, name := range targets {
		host, ok := a.hosts[name]
		if !ok {
			return observation{}, fmt.Errorf("%s has no inventory record, so there is no address to reach it at", name)
		}
		_, address := accessOf(host)

		// ⚠️ PROVEN AT THE MOMENT OF THE RUN, NEVER ASSUMED FROM THE
		// RECORD. A record naming an address is a declaration that
		// somebody believes a machine is there; it is not evidence that it
		// is. Reading it back as evidence would be this gate failing open
		// with the declaration vouching for itself -- see internal/reach's
		// package doc for what a probe does and does not prove, and why
		// the play's own --check pre-run is what answers the rest.
		answered, err := a.prober.Answers(ctx, address)
		if err != nil {
			// A malformed address is a broken record, not a machine that
			// is down, and the two must not be reported the same way: one
			// is a JSON file somebody has to fix, the other might be back
			// in five minutes. reach.Answers keeps them apart and so does
			// this.
			return observation{}, fmt.Errorf("%s: %v", name, err)
		}
		if !answered {
			unreachable = append(unreachable, name)
		}
	}
	return observation{Unreachable: unreachable}, nil
}

// --- wiring -------------------------------------------------------------

// evidenceProviders builds the providers available to one pass.
//
// ⚠️ THE ADDRESS PROVIDER IS ALWAYS AVAILABLE AND THE TAILSCALE ONE IS NOT,
// AND THAT DIFFERENCE IS A FACT ABOUT CREDENTIALS RATHER THAN A PREFERENCE.
// Dialling an address the reviewed inventory states needs nothing mounted;
// listing a tailnet needs an API key, and most deployments have neither the
// key nor a tailnet. Demanding it at startup would stop them on upgrade for
// a feature they never asked for, so it is absent here and the refusal
// lands on the commit that first declares a host reached that way -- where
// it is both correct and actionable.
func evidenceProviders(d applyDeps, snap inventory.Snapshot) evidenceSet {
	set := evidenceSet{
		inventory.AccessAddress: addressEvidence{
			hosts:  snap.Hosts,
			prober: reach.Prober{Dial: d.Dial},
		},
	}
	if d.Tailnet != nil {
		set[inventory.AccessTailscale] = tailscaleEvidence{lister: d.Tailnet, now: d.now}
	}
	return set
}

// dialer is the signature applyDeps.Dial carries, named so the field and
// reach.Prober.Dial cannot drift apart silently.
type dialer func(ctx context.Context, network, address string) (net.Conn, error)
