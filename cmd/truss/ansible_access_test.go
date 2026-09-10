package main

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/beeradb/truss/internal/tailnet"
)

// --- a dialer with no network behind it ---------------------------------

// fakeDial answers for exactly the addresses it was given and records every
// address it was asked for.
//
// ⚠️ IT RECORDS, BECAUSE "WHICH MACHINES DID THE APPLIER TOUCH" IS ITSELF A
// PROPERTY UNDER TEST. A gate that proves a host is up by dialling it is
// reach the applier did not have before, and it must reach exactly the
// hosts this commit is about and no others.
type fakeDial struct {
	mu       sync.Mutex
	answers  map[string]bool
	attempts []string
}

func dialing(answering ...string) *fakeDial {
	f := &fakeDial{answers: map[string]bool{}}
	for _, a := range answering {
		f.answers[a] = true
	}
	return f
}

func (f *fakeDial) dial(_ context.Context, _, address string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, address)
	if f.answers[address] {
		return &closedConn{}, nil
	}
	return nil, errors.New("connection refused")
}

func (f *fakeDial) dialled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.attempts...)
	sort.Strings(out)
	return out
}

// closedConn is a net.Conn that only supports Close. Every other method
// panics on purpose: a probe that read or wrote a byte would be doing
// something this design says it does not do, and the test should say so
// loudly rather than pass.
type closedConn struct{ net.Conn }

func (*closedConn) Close() error { return nil }

// accessFixture is ansibleFixture with an inventory whose hosts state how
// they are reached, and with a dialer instead of a network.
func accessFixture(t *testing.T, sha string, hosts map[string]accessHost, plays []string, dial *fakeDial) (applyDeps, *fakeAnsible) {
	t.Helper()
	deps, fake, _ := ansibleFixture(t, sha, []string{plays[0] + "/site.yml"}, invWithAccess(hosts), plays)
	deps.Dial = dial.dial
	return deps, fake
}

// --- the capability this change exists for ------------------------------

// TestAHostReachedAtItsDeclaredAddressRunsWithNoTailscaleCredential IS THE
// REPRODUCTION OF WHAT THIS CHANGE FIXES, and it fails outright against the
// previous code, which refused any play at all when d.Tailnet was nil.
//
// Two things were wrong with that. A truss user who does not run Tailscale
// could not run an ansible unit -- one deployment's process compiled into a
// general engine. And first contact was impossible in the only order that
// works: a host that has never joined a tailnet has no device record, so it
// could never be a target, so the play that would have joined it could
// never run.
func TestAHostReachedAtItsDeclaredAddressRunsWithNoTailscaleCredential(t *testing.T) {
	const sha = "commitsha-address"
	dial := dialing("dev-agent.example.invalid:22")
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dial)
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a deployment with no tailnet must still be able to configure a machine it can reach", result.failure)
	}
	if !fake.applied() {
		t.Fatalf("calls = %v, want the play to have run", fake.calls)
	}
	if got := dial.dialled(); len(got) == 0 {
		t.Fatal("nothing was dialled: reachability must be PROVEN at the moment of the run, never assumed from the record")
	}
}

// TestADeclaredAddressThatAnswersNothingRefusesRatherThanSkips. Absent is
// not "fine" for a host the inventory says this play configures -- the same
// rule that governs a host missing from the tailnet, and it must not have
// been softened on the way through a new provider.
func TestADeclaredAddressThatAnswersNothingRefusesRatherThanSkips(t *testing.T) {
	const sha = "commitsha-address-down"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing())
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "dev-agent") || !strings.Contains(result.failure, "unreachable") {
		t.Fatalf("result.failure = %q, want it to name dev-agent as unreachable", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestARecordIsADeclarationAndNotEvidence. The record naming an address is
// exactly as present when the machine is up as when it is not; if the gate
// could be satisfied by the record alone it would be the declaration
// vouching for itself, which is the target gate failing open in the one
// place there is no digest to fall back on.
func TestARecordIsADeclarationAndNotEvidence(t *testing.T) {
	const sha = "commitsha-address-declaration"
	hosts := map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
	}

	up, fakeUp := accessFixture(t, sha, hosts, []string{"ansible/plays/dev-vm"}, dialing("dev-agent.example.invalid:22"))
	up.Tailnet = nil
	if result := runOnePass(t, up, sha); result.failure != "" {
		t.Fatalf("with the machine answering: result.failure = %q, want empty", result.failure)
	}
	if !fakeUp.applied() {
		t.Fatal("with the machine answering: the play did not run")
	}

	down, fakeDown := accessFixture(t, sha, hosts, []string{"ansible/plays/dev-vm"}, dialing())
	down.Tailnet = nil
	if result := runOnePass(t, down, sha); result.failure == "" {
		t.Fatal("with the machine down: the pass succeeded on an identical inventory record -- the record was read as evidence")
	}
	if fakeDown.applied() {
		t.Fatal("with the machine down: the play ran anyway")
	}
}

// TestAMalformedAddressFailsThePassAndIsNotReportedAsAHostBeingDown. A typo
// in a JSON file and a machine that is off are different problems with
// different fixes, and telling an operator to go and look at a host that is
// fine is the wrong one.
func TestAMalformedAddressFailsThePassAndIsNotReportedAsAHostBeingDown(t *testing.T) {
	const sha = "commitsha-address-typo"
	dial := dialing()
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "ssh://dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dial)
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "dev-agent") {
		t.Fatalf("result.failure = %q, want it to name the host whose record is broken", result.failure)
	}
	if strings.Contains(result.failure, "unreachable") {
		t.Fatalf("result.failure = %q, want it NOT to read as a host being down: the record cannot be dialled at all", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
	if got := dial.dialled(); len(got) != 0 {
		t.Fatalf("dialled %v, want nothing: a record that cannot be parsed must not produce a connection somewhere else", got)
	}
}

// --- the invariant that must not have been weakened ---------------------

// TestNoProviderCanVouchForAHostAndThePassRefuses. Making evidence
// pluggable is not making it optional: KindAnsible has no plan digest, so
// the target set is the whole gate, and a target set nothing can vouch for
// is a gate that passes.
func TestNoProviderCanVouchForAHostAndThePassRefuses(t *testing.T) {
	const sha = "commitsha-unvouchable"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "tailscale"},
	}, []string{"ansible/plays/dev-vm"}, dialing())
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no tailscale credential") || !strings.Contains(result.failure, "dev-agent") {
		t.Fatalf("result.failure = %q, want it to name the host and the evidence it needed", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAnEmptyDeclaredSetIsRefusedWhicheverProviderIsInUse. A play targeting
// nothing has broken inventory wiring, and ansible with no --limit runs
// against every host it can see. The refusal is a property of the target
// set, so it cannot depend on how the hosts would have been reached.
func TestAnEmptyDeclaredSetIsRefusedWhicheverProviderIsInUse(t *testing.T) {
	const sha = "commitsha-address-nohosts"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/other", via: "address", address: "dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing("dev-agent.example.invalid:22"))
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no declared hosts") {
		t.Fatalf("result.failure = %q, want the empty-target-set refusal", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestATailnetIntruderStillRefusesAPlayWhoseOwnHostsAreReachedByAddress. An
// undeclared machine is a fleet-wide fact and refuses every play in the
// pass, including plays that name no tailnet-reached host at all. The
// tempting optimisation -- only ask the providers some host names -- would
// let a pass be blinded by moving every host onto another provider.
func TestATailnetIntruderStillRefusesAPlayWhoseOwnHostsAreReachedByAddress(t *testing.T) {
	const sha = "commitsha-mixed-intruder"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing("dev-agent.example.invalid:22"))
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("stowaway", managedTag)}}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "stowaway") {
		t.Fatalf("result.failure = %q, want it to name the undeclared device even though no host in this play is on the tailnet", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// --- the two providers do not judge each other's hosts ------------------

// TestAnAddressReachedHostIsNotJudgedByTheTailnet. tailnet.Reconcile is
// handed the WHOLE managed set, because an undeclared machine is only
// undeclared relative to every record there is -- and the other side of
// that same call then reports every address-reached host that is not on the
// tailnet as unreachable. It is not the tailnet's judgement to make: that
// machine was never claimed to be there, and the probe that does vouch for
// it has its own answer.
func TestAnAddressReachedHostIsNotJudgedByTheTailnet(t *testing.T) {
	const sha = "commitsha-mixed-clean"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "tailscale"},
		"dev-edge":  {play: "ansible/plays/dev-vm", via: "address", address: "dev-edge.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing("dev-edge.example.invalid:22"))
	// dev-edge is deliberately absent from the tailnet: it is reached at an
	// address and has no business being there.
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("dev-agent", managedTag)}}

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- an address-reached host missing from the tailnet is not unreachable, it is somewhere else", result.failure)
	}
	if !fake.applied() {
		t.Fatalf("calls = %v, want the play to have run against both hosts", fake.calls)
	}
	for _, c := range fake.calls {
		if !strings.HasSuffix(c, " dev-agent,dev-edge") {
			t.Fatalf("call %q, want both hosts on the limit", c)
		}
	}
}

// TestATailnetReachedHostIsNotDialled. The converse: a host reached over
// the tailnet must not acquire a second, unreviewed reachability check, and
// the applier must not be opening connections to machines whose records
// never named an address.
func TestATailnetReachedHostIsNotDialled(t *testing.T) {
	const sha = "commitsha-mixed-nodial"
	dial := dialing("dev-edge.example.invalid:22")
	deps, _ := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "tailscale"},
		"dev-edge":  {play: "ansible/plays/dev-vm", via: "address", address: "dev-edge.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dial)
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("dev-agent", managedTag)}}

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	got := dial.dialled()
	if len(got) != 1 || got[0] != "dev-edge.example.invalid:22" {
		t.Fatalf("dialled %v, want only the address-reached host", got)
	}
}

// TestNothingIsDialledForAHostThisCommitWillNotConfigure. A provider that
// has to touch a machine to observe it touches only the machines the plays
// about to run actually name -- a frozen host, a decommissioned one, or a
// host belonging to some other play is not this commit's business, and its
// answer would be discarded unread anyway.
func TestNothingIsDialledForAHostThisCommitWillNotConfigure(t *testing.T) {
	const sha = "commitsha-address-scope"
	dial := dialing("dev-agent.example.invalid:22")
	deps, _ := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
		"dev-cold":  {play: "ansible/plays/dev-vm", via: "address", address: "dev-cold.example.invalid", frozen: true},
		"dev-gone":  {play: "ansible/plays/dev-vm", via: "address", address: "dev-gone.example.invalid", decommissioned: true},
		"dev-other": {play: "ansible/plays/other", via: "address", address: "dev-other.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dial)
	deps.Tailnet = nil

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	got := dial.dialled()
	if len(got) != 1 || got[0] != "dev-agent.example.invalid:22" {
		t.Fatalf("dialled %v, want only the one host this play will configure", got)
	}
}

// --- the disclosure -----------------------------------------------------

// TestTheApplierSaysWhichGuaranteeThePassGave. A declared-address provider
// cannot discover a machine claiming to be managed that nobody declared,
// and that is a real reduction in safety rather than an implementation gap.
// It is not a refusal -- the provider is first class -- so what it gets is
// disclosure, on every pass, in the same posture a frozen host gets: named
// every time, never silently skipped.
func TestTheApplierSaysWhichGuaranteeThePassGave(t *testing.T) {
	const sha = "commitsha-address-blind"
	deps, _ := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "address", address: "dev-agent.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing("dev-agent.example.invalid:22"))
	deps.Tailnet = nil
	logs := &strings.Builder{}
	deps.Stderr = logs

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if !strings.Contains(logs.String(), "cannot discover") {
		t.Fatalf("stderr = %q, want the pass to disclose that nothing in it could look for an undeclared machine", logs.String())
	}
}

// TestATailnetOnlyPassDisclosesNothing, because a limitation stated to a
// deployment that does not have it is a line that trains an operator to
// skip the line -- the same reason a decommissioned host stays off the
// unreachable list.
func TestATailnetOnlyPassDisclosesNothing(t *testing.T) {
	const sha = "commitsha-tailscale-clean"
	deps, _ := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "tailscale"},
	}, []string{"ansible/plays/dev-vm"}, dialing())
	logs := &strings.Builder{}
	deps.Stderr = logs

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if strings.Contains(logs.String(), "cannot discover") {
		t.Fatalf("stderr = %q, want no disclosure: every provider this pass used could look", logs.String())
	}
}

// --- the migration tolerance -------------------------------------------

// TestAHostWithNoAccessBlockIsStillReachedOverTheTailnet pins the one
// unstated field this system reads as an answer, so it cannot be widened
// into a general fallback and cannot be tightened without somebody deleting
// a test that explains why.
//
// Every record written before the field existed was necessarily reached
// over the tailnet -- it was the only thing the applier could reach a
// machine through -- so this is the only default that cannot silently
// change what an existing commit does. See inventory.Host.Access.
func TestAHostWithNoAccessBlockIsStillReachedOverTheTailnet(t *testing.T) {
	const sha = "commitsha-noaccess"
	dial := dialing()
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm"},
	}, []string{"ansible/plays/dev-vm"}, dial)

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if !fake.applied() {
		t.Fatalf("calls = %v, want the play to have run", fake.calls)
	}
	if got := dial.dialled(); len(got) != 0 {
		t.Fatalf("dialled %v, want nothing: an unstated access block means the tailnet, not an address", got)
	}
}

// TestAHostWithNoAccessBlockAndNoTailscaleCredentialStillRefuses is why the
// tolerance above cannot fail open. Reading nil as "tailscale" answers WHICH
// provider vouches for the host; it never answers WHETHER one can.
func TestAHostWithNoAccessBlockAndNoTailscaleCredentialStillRefuses(t *testing.T) {
	const sha = "commitsha-noaccess-nocred"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm"},
	}, []string{"ansible/plays/dev-vm"}, dialing())
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no tailscale credential") {
		t.Fatalf("result.failure = %q, want the missing-evidence refusal", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAnAddressReachedHostOnTheTailnetIsNotMistakenForAnIntruder is the
// other side of measuring against the whole fleet, and it is the one that
// bites. A machine can perfectly well be reached at a stated address AND be
// on the tailnet wearing the managed tag -- that is the steady state after
// first contact, before somebody edits the record. Compare the device list
// against only the tailnet-vouched names and that host is a device carrying
// the managed tag with no matching record, which refuses every play in the
// pass over a host somebody did write down.
func TestAnAddressReachedHostOnTheTailnetIsNotMistakenForAnIntruder(t *testing.T) {
	const sha = "commitsha-joined"
	deps, fake := accessFixture(t, sha, map[string]accessHost{
		"dev-agent": {play: "ansible/plays/dev-vm", via: "tailscale"},
		"dev-edge":  {play: "ansible/plays/dev-vm", via: "address", address: "dev-edge.example.invalid"},
	}, []string{"ansible/plays/dev-vm"}, dialing("dev-edge.example.invalid:22"))
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{
		liveDevice("dev-agent", managedTag),
		liveDevice("dev-edge", managedTag),
	}}

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- dev-edge is declared, and how it is REACHED says nothing about whether it was DECLARED", result.failure)
	}
	if !fake.applied() {
		t.Fatalf("calls = %v, want the play to have run", fake.calls)
	}
}
