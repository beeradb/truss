package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/beeradb/truss/internal/inventory"
)

// --- fakes --------------------------------------------------------------

// fakeEvidence is a provider with nothing behind it, so gatherEvidence's
// merging and its refusals can be driven without a tailnet or a socket.
type fakeEvidence struct {
	name      string
	discovers bool
	obs       observation
	err       error
	// sawFleet and sawTargets record the two arguments, which are the
	// asymmetry this whole design exists to make visible -- a test that did
	// not look at them could not tell the two providers apart.
	sawFleet   []string
	sawTargets []string
	calls      int
}

func (f *fakeEvidence) Name() string    { return f.name }
func (f *fakeEvidence) Discovers() bool { return f.discovers }
func (f *fakeEvidence) Observe(ctx context.Context, fleet, targets []string) (observation, error) {
	f.calls++
	f.sawFleet = append([]string(nil), fleet...)
	f.sawTargets = append([]string(nil), targets...)
	return f.obs, f.err
}

// evidenceDeps is the smallest applyDeps gatherEvidence reads: a clock and
// somewhere to narrate to.
func evidenceDeps(logs *strings.Builder) applyDeps {
	return applyDeps{Stderr: logs, Now: func() time.Time { return passTime }}
}

// --- the asymmetry ------------------------------------------------------

// TestAProviderThatCannotDiscoverIsNotReadAsHavingFoundNone IS THE POINT OF
// THIS DESIGN, stated as a test.
//
// Tailscale can be asked "which machines claim to be managed?" and answer
// with ones nobody declared. A declared-address provider cannot: the only
// addresses it knows are the ones the inventory already names, so the set
// it can look at IS the declared set. Both providers return zero undeclared
// machines, and the two zeroes mean completely different things -- "we
// looked and the fleet is clean" against "nothing here can look". Anything
// that collapsed them would turn the most valuable half of the ansible
// target gate off in silence.
func TestAProviderThatCannotDiscoverIsNotReadAsHavingFoundNone(t *testing.T) {
	logs := &strings.Builder{}
	blind := &fakeEvidence{name: "address", discovers: false}
	ev, reason := gatherEvidence(context.Background(), evidenceDeps(logs), evidenceSet{"address": blind},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "address", address: "dev-agent.example.invalid"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if len(ev.Undeclared) != 0 {
		t.Fatalf("Undeclared = %v, want none", ev.Undeclared)
	}
	if len(ev.Blind) != 1 || ev.Blind[0] != "address" {
		t.Fatalf("Blind = %v, want exactly [address] -- an empty Undeclared from a provider that cannot look must not read as a clean fleet", ev.Blind)
	}
	if !strings.Contains(logs.String(), "cannot discover") {
		t.Fatalf("stderr = %q, want the pass to say which guarantee it gave", logs.String())
	}
}

// TestAProviderThatLookedAndFoundNoneIsDistinguishable is the other half,
// and without it the test above passes against code that reports every
// provider as blind.
func TestAProviderThatLookedAndFoundNoneIsDistinguishable(t *testing.T) {
	logs := &strings.Builder{}
	seeing := &fakeEvidence{name: "tailscale", discovers: true}
	ev, reason := gatherEvidence(context.Background(), evidenceDeps(logs), evidenceSet{"tailscale": seeing},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "tailscale"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if len(ev.Undeclared) != 0 || len(ev.Blind) != 0 {
		t.Fatalf("Undeclared = %v, Blind = %v, want both empty -- this provider looked and the fleet was clean", ev.Undeclared, ev.Blind)
	}
	if strings.Contains(logs.String(), "cannot discover") {
		t.Fatalf("stderr = %q, want no disclosure: every provider in this pass could look", logs.String())
	}
}

// TestADiscoveringProviderIsAskedEvenWhenNoHostNamesIt. An undeclared
// machine hides precisely by not being declared, so asking only the
// providers some record points at would let a pass be blinded by the very
// records it is checking: move every host onto another provider and the
// intruder search silently stops happening.
func TestADiscoveringProviderIsAskedEvenWhenNoHostNamesIt(t *testing.T) {
	seeing := &fakeEvidence{name: "tailscale", discovers: true, obs: observation{Undeclared: []string{"stowaway"}}}
	blind := &fakeEvidence{name: "address", discovers: false}

	ev, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}),
		evidenceSet{"tailscale": seeing, "address": blind},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "address", address: "dev-agent.example.invalid"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if seeing.calls != 1 {
		t.Fatalf("the discovering provider was asked %d times, want 1 -- no host names it and it must still be asked", seeing.calls)
	}
	if len(ev.Undeclared) != 1 || ev.Undeclared[0] != "stowaway" {
		t.Fatalf("Undeclared = %v, want the intruder it found", ev.Undeclared)
	}
}

// TestABlindProviderNoHostNamesIsNotAskedAndDoesNotDiscloseAnything. The
// converse keeps the disclosure worth reading: a deployment with no
// address-reached host must not be told every pass about a limitation it
// does not have, or the line stops being read -- the same rule that keeps a
// decommissioned host off the unreachable list.
func TestABlindProviderNoHostNamesIsNotAskedAndDoesNotDiscloseAnything(t *testing.T) {
	logs := &strings.Builder{}
	seeing := &fakeEvidence{name: "tailscale", discovers: true}
	blind := &fakeEvidence{name: "address", discovers: false}

	ev, reason := gatherEvidence(context.Background(), evidenceDeps(logs),
		evidenceSet{"tailscale": seeing, "address": blind},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "tailscale"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if blind.calls != 0 {
		t.Fatalf("the address provider was asked %d times, want 0 -- no host is reached that way", blind.calls)
	}
	if len(ev.Blind) != 0 {
		t.Fatalf("Blind = %v, want empty", ev.Blind)
	}
	if strings.Contains(logs.String(), "cannot discover") {
		t.Fatalf("stderr = %q, want no disclosure about a provider this deployment does not use", logs.String())
	}
}

// TestAProviderContradictingItsOwnCapabilityRefuses. Merging the names
// would trust a provider that disagrees with itself; dropping them would
// hide an intruder it somehow did find. Either way the pass would be acting
// on a component that does not know what it is, so it stops.
func TestAProviderContradictingItsOwnCapabilityRefuses(t *testing.T) {
	liar := &fakeEvidence{name: "address", discovers: false, obs: observation{Undeclared: []string{"stowaway"}}}
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}), evidenceSet{"address": liar},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "address", address: "dev-agent.example.invalid"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if !strings.Contains(reason, "stowaway") || !strings.Contains(reason, "contradicts its own capability") {
		t.Fatalf("reason = %q, want a refusal naming the machine and the contradiction", reason)
	}
}

// --- what each provider is asked ---------------------------------------

// TestADiscoveringProviderIsMeasuredAgainstTheWHOLEFleet. A machine
// vouched for by another provider is still declared: a tailnet compared
// against only the tailnet-vouched names would report every
// address-reached host on it as an intruder, and refuse every play in the
// pass over a host somebody did write down.
func TestADiscoveringProviderIsMeasuredAgainstTheWHOLEFleet(t *testing.T) {
	seeing := &fakeEvidence{name: "tailscale", discovers: true}
	hosts := map[string]accessHost{
		"dev-agent": {play: "p", via: "tailscale"},
		"dev-edge":  {play: "p", via: "address", address: "dev-edge.example.invalid"},
	}
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}),
		evidenceSet{"tailscale": seeing, "address": &fakeEvidence{name: "address"}},
		snapshotOf(hosts),
		[]playPlan{{play: "p", declared: []string{"dev-agent", "dev-edge"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if strings.Join(seeing.sawFleet, ",") != "dev-agent,dev-edge" {
		t.Fatalf("fleet = %v, want every managed host whichever provider vouches for it", seeing.sawFleet)
	}
	if strings.Join(seeing.sawTargets, ",") != "dev-agent" {
		t.Fatalf("targets = %v, want only the hosts this provider is responsible for", seeing.sawTargets)
	}
}

// TestAProviderIsGivenOnlyTheHostsThisCommitWillConfigure. A provider that
// must touch a machine to observe it must touch only the machines this
// commit is about: dialling a host no play in this commit configures is
// reach nothing asked for, and the answer would be discarded unread.
func TestAProviderIsGivenOnlyTheHostsThisCommitWillConfigure(t *testing.T) {
	blind := &fakeEvidence{name: "address"}
	hosts := map[string]accessHost{
		"dev-agent": {play: "p", via: "address", address: "dev-agent.example.invalid"},
		"dev-other": {play: "q", via: "address", address: "dev-other.example.invalid"},
	}
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}), evidenceSet{"address": blind},
		snapshotOf(hosts),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if reason != "" {
		t.Fatalf("gatherEvidence refused: %s", reason)
	}
	if strings.Join(blind.sawTargets, ",") != "dev-agent" {
		t.Fatalf("targets = %v, want only the host the running play names", blind.sawTargets)
	}
	if strings.Join(blind.sawFleet, ",") != "dev-agent,dev-other" {
		t.Fatalf("fleet = %v, want every managed host", blind.sawFleet)
	}
}

// --- refusals -----------------------------------------------------------

// TestAHostWhoseProviderIsNotMountedRefusesBeforeAnythingIsObserved. This
// is the invariant that must survive evidence becoming pluggable: a play is
// gated by its target set and nothing else, so a target nothing can vouch
// for is a gate that passes.
func TestAHostWhoseProviderIsNotMountedRefusesBeforeAnythingIsObserved(t *testing.T) {
	blind := &fakeEvidence{name: "address"}
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}), evidenceSet{"address": blind},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "tailscale"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if !strings.Contains(reason, "dev-agent") || !strings.Contains(reason, "no tailscale credential") {
		t.Fatalf("reason = %q, want it to name the host and the credential it needed", reason)
	}
	if blind.calls != 0 {
		t.Fatalf("a provider was asked %d times before the refusal; nothing may be observed once the target set is already unvouchable", blind.calls)
	}
}

// TestAnUnrecognisedViaRefusesAndOffersTheOnesThatExist. inventory.Check
// refuses this too, and the duplication is deliberate for the reason the
// "inventory does not load" refusal beside it states: this function is
// called directly by tests and could be by a future caller, and reading
// hosts out of a record nobody can interpret is how a play acquires a
// target set nobody wrote.
func TestAnUnrecognisedViaRefusesAndOffersTheOnesThatExist(t *testing.T) {
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}), evidenceSet{},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "carrier-pigeon"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if !strings.Contains(reason, "carrier-pigeon") || !strings.Contains(reason, "does not understand") {
		t.Fatalf("reason = %q, want it to name the unreadable access", reason)
	}
	if !strings.Contains(reason, "tailscale") || !strings.Contains(reason, "address") {
		t.Fatalf("reason = %q, want it to offer the values that do exist", reason)
	}
}

// TestAProviderThatCouldNotLookFailsThePass. An unreadable provider is not
// an empty one -- internal/secrets.Store.List records the same rule for an
// unreadable store -- and continuing on an error would run a play against
// hosts nothing observed.
func TestAProviderThatCouldNotLookFailsThePass(t *testing.T) {
	broken := &fakeEvidence{name: "tailscale", discovers: true, err: errors.New("401 unauthorized")}
	_, reason := gatherEvidence(context.Background(), evidenceDeps(&strings.Builder{}), evidenceSet{"tailscale": broken},
		snapshotOf(map[string]accessHost{"dev-agent": {play: "p", via: "tailscale"}}),
		[]playPlan{{play: "p", declared: []string{"dev-agent"}}}, "sha")

	if !strings.Contains(reason, "401 unauthorized") || !strings.Contains(reason, "tailscale") {
		t.Fatalf("reason = %q, want it to name the provider and what went wrong", reason)
	}
}

// --- fixtures -----------------------------------------------------------

// accessHost is one host record for a fixture: which play configures it and
// how the applier reaches it. An empty via leaves the access block out
// entirely, which is the pre-field shape every existing record has.
type accessHost struct {
	play           string
	via            string
	address        string
	frozen         bool
	decommissioned bool
}

// invWithAccess builds an inventory tree whose hosts carry access blocks.
// It is invWithPlay's sibling and deliberately not a change to it: the
// tests already written against that one are about a world where every host
// was reached the same way, and they must keep testing that world.
func invWithAccess(hosts map[string]accessHost) fstest.MapFS {
	out := fstest.MapFS{}
	for name, h := range hosts {
		access := "null"
		if h.via != "" {
			access = `{"via":"` + h.via + `"`
			if h.address != "" {
				access += `,"address":"` + h.address + `"`
			}
			access += `}`
		}
		out["inventory/hosts/"+name+".json"] = &fstest.MapFile{Data: []byte(`{
			"schema":"truss.host/v1","name":"` + name + `","kind":"vm","role":"dev",
			"provisioned_by":"hosts/` + name + `","config":"` + h.play + `",
			"tailnet_tags":["managed"],"cluster":null,
			"frozen":` + boolText(h.frozen) + `,"decommissioned":` + boolText(h.decommissioned) + `,
			"access":` + access + `}`)}
	}
	return out
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// snapshotOf loads invWithAccess's tree, so the unit tests above are driven
// by records that went through the real decoder rather than by structs
// built by hand -- a fixture more forgiving than production invents
// failures, and one stricter hides them.
func snapshotOf(hosts map[string]accessHost) inventory.Snapshot {
	snap, problems := inventory.Load(invWithAccess(hosts))
	if len(problems) > 0 {
		panic("evidence_test fixture does not load: " + strings.Join(problems, "; "))
	}
	return snap
}
