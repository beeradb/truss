package main

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/beeradb/truss/internal/ansible"
	"github.com/beeradb/truss/internal/inventory"
	"github.com/beeradb/truss/internal/tailnet"
)

// unconfiguredAnsible is buildTestDeps' default runner: it refuses by name
// rather than reporting a clean run, the same shape unconfiguredRender uses.
type unconfiguredAnsible struct{}

func (unconfiguredAnsible) Check(ctx context.Context, dir string, targets []ansible.Target) (ansible.Result, error) {
	return ansible.Result{}, errors.New("this fixture did not configure an ansible runner; set deps.NewAnsible")
}

func (unconfiguredAnsible) Apply(ctx context.Context, dir string, targets []ansible.Target) (ansible.Result, error) {
	return ansible.Result{}, errors.New("this fixture did not configure an ansible runner; set deps.NewAnsible")
}

// fakeAnsible records every invocation in order, so a test can assert both
// WHAT ran and WHETHER IT RAN AT ALL -- the second being the property most of
// this file is about, because a gate that refuses after the play has already
// configured six machines has not refused anything.
// targetNames is the host names of a target list, so a fake can keep
// recording calls as "check <dir> <host,host>" now that the runner is
// handed addresses and groups as well as names.
func targetNames(ts []ansible.Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

type fakeAnsible struct {
	// calls is one entry per invocation: "check <dir> <host,host>" or
	// "apply <dir> <host,host>". A single ordered list rather than two
	// slices, because the ORDER between check and apply is itself a
	// property under test -- check mode must run first.
	calls []string
	// checkResults are returned by successive Check calls; the last one is
	// reused once exhausted, so a fixture that cares only about the pre-run
	// supplies one.
	checkResults []ansible.Result
	checkErrs    []error
	applyErr     error
}

func (f *fakeAnsible) Check(ctx context.Context, dir string, targets []ansible.Target) (ansible.Result, error) {
	n := f.count("check")
	f.calls = append(f.calls, "check "+dir+" "+strings.Join(targetNames(targets), ","))
	if n < len(f.checkErrs) && f.checkErrs[n] != nil {
		return ansible.Result{}, f.checkErrs[n]
	}
	if len(f.checkResults) == 0 {
		return ansible.Result{ChangedByHost: map[string]int{}}, nil
	}
	if n >= len(f.checkResults) {
		n = len(f.checkResults) - 1
	}
	return f.checkResults[n], nil
}

func (f *fakeAnsible) Apply(ctx context.Context, dir string, targets []ansible.Target) (ansible.Result, error) {
	f.calls = append(f.calls, "apply "+dir+" "+strings.Join(targetNames(targets), ","))
	if f.applyErr != nil {
		return ansible.Result{}, f.applyErr
	}
	return ansible.Result{ChangedByHost: map[string]int{}}, nil
}

func (f *fakeAnsible) count(verb string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, verb+" ") {
			n++
		}
	}
	return n
}

// applied reports whether any play was actually run for real. Nearly every
// refusal test below asserts this is false: the whole point of the target
// gate is that it decides BEFORE a machine is touched.
func (f *fakeAnsible) applied() bool { return f.count("apply") > 0 }

// fakeTailnet is a device list with no network behind it.
type fakeTailnet struct {
	devices []tailnet.Device
	err     error
}

func (f fakeTailnet) Devices(ctx context.Context) ([]tailnet.Device, error) {
	return f.devices, f.err
}

// passTime is buildTestDeps' frozen clock. Every device below is "seen" at
// exactly that instant unless a test is about staleness.
var passTime = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func liveDevice(name string, tags ...string) tailnet.Device {
	return tailnet.Device{Name: name, Tags: tags, LastSeen: passTime}
}

// invWithPlay builds a consistent inventory whose hosts are exactly those
// given: name -> the play that configures it. Frozen and decommissioned
// hosts are spelled with a "!" or "-" prefix on the name, so a fixture reads
// as one line.
//
// It carries no clusters, projects or environments at all, which
// inventory.Check accepts: a platform of bare machines with no Kubernetes on
// them is a real shape, and it is exactly the shape a dev VM is onboarded
// into first.
func invWithPlay(hosts map[string]string) fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	out := fstest.MapFS{}
	for spec, play := range hosts {
		name, frozen, decommissioned := spec, "false", "false"
		switch {
		case strings.HasPrefix(spec, "!"):
			name, frozen = spec[1:], "true"
		case strings.HasPrefix(spec, "-"):
			name, decommissioned = spec[1:], "true"
		}
		out["inventory/hosts/"+name+".json"] = f(`{
			"schema":"truss.host/v1","name":"` + name + `","kind":"vm","role":"dev",
			"provisioned_by":"hosts/` + name + `","config":"` + play + `",
			"tailnet_tags":["managed"],"cluster":null,
			"frozen":` + frozen + `,"decommissioned":` + decommissioned + `}`)
	}
	return out
}

// ansibleFixture wires a pass around one commit that touches one play.
func ansibleFixture(t *testing.T, sha string, changed []string, tree fs.FS, plays []string) (applyDeps, *fakeAnsible, *fakeLedger) {
	t.Helper()
	forgeFake := compliantCommitGate("alice", sha, sha)
	forgeFake.ProtectionResult = compliantGatesProtection()

	inTree := map[string]bool{}
	for _, p := range plays {
		inTree[p] = true
	}
	git := &fakeGit{
		CommitsList:              []string{sha},
		ChangedByCommit:          map[string][]string{sha: changed},
		TreeRootsByCommit:        map[string][]string{sha: nil},
		TreeAnsibleUnitsByCommit: map[string][]string{sha: plays},
		TreeFSBySha:              map[string]fs.FS{sha: tree},
		HasDirFn:                 func(dir string) bool { return inTree[dir] },
	}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return &fakeTofu{} })
	fake := &fakeAnsible{}
	deps.NewAnsible = func(env []string) ansibleRunner { return fake }
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("dev-agent", managedTag)}}
	return deps, fake, fl
}

func runOnePass(t *testing.T, deps applyDeps, head string) applyResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runApplyPass(ctx, deps, head)
}

// TestACommitTouchingOnlyAPlayIsRunNotNooped IS THE REPRODUCTION OF THE
// DEFECT THIS FILE CLOSES. repo.KindOf has classified ansible/plays/<name>
// as KindAnsible since the kind layer landed, and nothing in the pass read
// it: such a commit derived no roots and no render units, was logged as
// "touches no root", and had HEAD advanced past it. The machine it was meant
// to configure was never configured, and the ledger said the commit was
// uneventful.
func TestACommitTouchingOnlyAPlayIsRunNotNooped(t *testing.T) {
	const sha = "commitsha-play"
	deps, fake, fl := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if !fake.applied() {
		t.Fatalf("calls = %v, want the play to have been run -- it was filed as a noop instead", fake.calls)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Fatalf("applied/%s was not written; the commit was recorded as a noop", sha)
	}
}

// TestAPlayRunsAgainstExactlyTheDeclaredHosts pins the argument that decides
// whether this configures one machine or the fleet. The inventory declares
// two hosts for this play and one for another; only the two may appear.
func TestAPlayRunsAgainstExactlyTheDeclaredHosts(t *testing.T) {
	const sha = "commitsha-limit"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{
			"dev-agent": "ansible/plays/dev-vm",
			"dev-two":   "ansible/plays/dev-vm",
			"builder":   "ansible/plays/build-box",
		}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{
		liveDevice("dev-agent", managedTag),
		liveDevice("dev-two", managedTag),
		liveDevice("builder", managedTag),
	}}

	if result := runOnePass(t, deps, sha); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	for _, c := range fake.calls {
		if !strings.HasSuffix(c, " dev-agent,dev-two") {
			t.Fatalf("call %q, want it to target exactly dev-agent,dev-two -- a play reaching a host the inventory did not hand it is the defect the target gate exists to prevent", c)
		}
	}
}

// TestAnUnknownTaggedDeviceRefusesEveryPlay is the fleet-wide direction: a
// machine wearing the managed tag that no inventory record names is either an
// intruder or a host somebody forgot, and neither is a reason to keep
// configuring the rest.
func TestAnUnknownTaggedDeviceRefusesEveryPlay(t *testing.T) {
	const sha = "commitsha-unknown"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{
		liveDevice("dev-agent", managedTag),
		liveDevice("stowaway", managedTag),
	}}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "stowaway") {
		t.Fatalf("result.failure = %q, want it to name the undeclared device", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want no play to have run at all", fake.calls)
	}
}

// TestADeclaredHostAbsentFromTheTailnetRefusesItsPlay is the other
// direction, and the reason it refuses rather than skips: absent is not
// "fine" for a host the inventory says this play configures.
func TestADeclaredHostAbsentFromTheTailnetRefusesItsPlay(t *testing.T) {
	const sha = "commitsha-absent"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = fakeTailnet{devices: nil}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "dev-agent") || !strings.Contains(result.failure, "unreachable") {
		t.Fatalf("result.failure = %q, want it to name dev-agent as unreachable", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAStaleDeviceCountsAsUnreachable pins the clock half of that check: a
// device that answered last week is on the list and is still gone.
func TestAStaleDeviceCountsAsUnreachable(t *testing.T) {
	const sha = "commitsha-stale"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{
		{Name: "dev-agent", Tags: []string{managedTag}, LastSeen: passTime.Add(-tailnetStaleAfter - time.Minute)},
	}}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "dev-agent") {
		t.Fatalf("result.failure = %q, want it to name the stale host", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestCheckModeRunsFirstAndItsFailureChangesNothing is why the pre-run
// exists: an error found in check mode is found while every machine is still
// untouched, rather than on host four of six.
func TestCheckModeRunsFirstAndItsFailureChangesNothing(t *testing.T) {
	const sha = "commitsha-checkfail"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	fake.checkErrs = []error{errors.New("exit status 4")}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "check mode refused") {
		t.Fatalf("result.failure = %q, want it to say check mode refused the play", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want no apply after a failed check", fake.calls)
	}
}

// TestConvergenceIsNamedNeverRefused: a host still reporting work straight
// after a successful run means a non-idempotent task, which is a defect in
// the play -- but the run succeeded and the machine is configured, so failing
// the pass here would wedge the queue behind a change that worked.
func TestConvergenceIsNamedNeverRefused(t *testing.T) {
	const sha = "commitsha-unconverged"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	fake.checkResults = []ansible.Result{
		{ChangedByHost: map[string]int{"dev-agent": 3}},
		{ChangedByHost: map[string]int{"dev-agent": 3}}, // still changing afterwards
	}
	logs := &strings.Builder{}
	deps.Stderr = logs

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- an unconverged host is named, not refused", result.failure)
	}
	if fake.count("check") != 2 {
		t.Fatalf("calls = %v, want a check before AND after the apply", fake.calls)
	}
	if !strings.Contains(logs.String(), "not idempotent") {
		t.Fatalf("stderr = %q, want it to name the play as non-idempotent", logs.String())
	}
}

// TestNoTailscaleCredentialRefusesAPlay. The target set is the whole gate
// here, because KindAnsible has no plan digest; with no evidence about which
// hosts exist there is no gate, and running anyway would be it failing open.
func TestNoTailscaleCredentialRefusesAPlay(t *testing.T) {
	const sha = "commitsha-notailnet"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = nil

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no tailscale credential") {
		t.Fatalf("result.failure = %q, want it to name the missing credential", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAPlayWithNoDeclaredHostsRefuses: a play no host names is broken
// inventory wiring, and the failure mode of running it anyway is the worst
// one available -- ansible with no --limit targets every host it can see.
func TestAPlayWithNoDeclaredHostsRefuses(t *testing.T) {
	const sha = "commitsha-nohosts"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/other"}),
		[]string{"ansible/plays/dev-vm"})
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("dev-agent", managedTag)}}

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no declared hosts") {
		t.Fatalf("result.failure = %q, want it to say the play has no declared hosts", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAFrozenHostIsSkippedAndNamedRatherThanRefused. Freezing is an
// operator's deliberate "do not touch"; refusing on it would wedge the queue,
// and skipping it silently would let somebody who forgot they set it wonder
// why their change never took effect.
func TestAFrozenHostIsSkippedAndNamedRatherThanRefused(t *testing.T) {
	const sha = "commitsha-frozen"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{
			"dev-agent": "ansible/plays/dev-vm",
			"!dev-cold": "ansible/plays/dev-vm",
		}),
		[]string{"ansible/plays/dev-vm"})
	// dev-cold is deliberately absent from the tailnet as well: a frozen
	// host must not be able to refuse the play it is excluded from.
	deps.Tailnet = fakeTailnet{devices: []tailnet.Device{liveDevice("dev-agent", managedTag)}}
	logs := &strings.Builder{}
	deps.Stderr = logs

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	for _, c := range fake.calls {
		if strings.Contains(c, "dev-cold") {
			t.Fatalf("call %q targeted a frozen host", c)
		}
	}
	if !strings.Contains(logs.String(), "dev-cold is frozen") {
		t.Fatalf("stderr = %q, want the frozen host named", logs.String())
	}
}

// TestADecommissionedHostIsNeitherConfiguredNorReportedUnreachable. Absent on
// purpose is the one absence that must not appear on the daily list, or the
// list stops being read.
func TestADecommissionedHostIsNeitherConfiguredNorReportedUnreachable(t *testing.T) {
	const sha = "commitsha-decom"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{
			"dev-agent": "ansible/plays/dev-vm",
			"-dev-gone": "ansible/plays/dev-vm",
		}),
		[]string{"ansible/plays/dev-vm"})

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a decommissioned host must not read as unreachable", result.failure)
	}
	for _, c := range fake.calls {
		if strings.Contains(c, "dev-gone") {
			t.Fatalf("call %q targeted a decommissioned host", c)
		}
	}
}

// TestARetiredPlayConfiguresNothingAndDoesNotRefuse. An absent play is the
// one case where the three kinds all differ: a root refuses, a delivery is
// pruned, a play is RETIRED -- deleting it does not un-configure anything,
// the machine keeps exactly what it has, and saying otherwise in either
// direction would be a lie about somebody's machine.
func TestARetiredPlayConfiguresNothingAndDoesNotRefuse(t *testing.T) {
	const sha = "commitsha-retired"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		invWithPlay(map[string]string{"dev-agent": "ansible/plays/dev-vm"}),
		[]string{"ansible/plays/dev-vm"})
	// The unit is in the diff and in the tree listing, but the checkout has
	// no such directory: the commit deleted it.
	deps.Git.(*fakeGit).HasDirFn = func(dir string) bool { return false }
	logs := &strings.Builder{}
	deps.Stderr = logs

	result := runOnePass(t, deps, sha)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none: a retired play runs nothing", fake.calls)
	}
	if !strings.Contains(logs.String(), "keep the configuration they already have") {
		t.Fatalf("stderr = %q, want the retirement named", logs.String())
	}
}

// TestATreeWithAPlayAndNoInventoryRefuses. checkInventoryAtCommit treats an
// absent inventory/ as "this deployment has not adopted the inventory" and
// returns clean, which is right for a tree that has none and never will. A
// tree with a PLAY and no inventory is a different fact: the play has no
// declared hosts, which is the one input that must never become "everything".
func TestATreeWithAPlayAndNoInventoryRefuses(t *testing.T) {
	const sha = "commitsha-noinv"
	deps, fake, _ := ansibleFixture(t, sha,
		[]string{"ansible/plays/dev-vm/site.yml"},
		fstest.MapFS{},
		[]string{"ansible/plays/dev-vm"})

	result := runOnePass(t, deps, sha)

	if !strings.Contains(result.failure, "no inventory/") {
		t.Fatalf("result.failure = %q, want it to name the missing inventory", result.failure)
	}
	if fake.applied() {
		t.Fatalf("calls = %v, want nothing to have run", fake.calls)
	}
}

// TestAPlayGetsNoCredential is the property the whole ansible kind rests on:
// a play's tasks run as root on somebody else's machine, so the environment
// they get must be exactly PATH and HOME and nothing the applier holds.
func TestAPlayGetsNoCredential(t *testing.T) {
	d := applyDeps{PATH: "/usr/bin", HOME: "/root"}
	got := ansibleEnv(d)
	want := []string{"PATH=/usr/bin", "HOME=/root"}
	if len(got) != len(want) {
		t.Fatalf("ansibleEnv = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ansibleEnv = %v, want exactly %v", got, want)
		}
	}
}

// TestAnsibleUnitsForReadsOnlyTheAnsibleHalf pins the partition the three
// tree listings rely on: whatever repo.TouchedUnits returns, this function
// returns none of the tofu or render units in it.
func TestAnsibleUnitsForReadsOnlyTheAnsibleHalf(t *testing.T) {
	changed := []string{
		"ansible/plays/dev-vm/site.yml",
		"hosts/dev-agent/main.tf",
		"deliveries/beta/web/kustomization.yaml",
	}
	got := ansibleUnitsFor(changed, []string{"ansible/plays/dev-vm"})
	if len(got) != 1 || got[0] != "ansible/plays/dev-vm" {
		t.Fatalf("ansibleUnitsFor = %v, want exactly [ansible/plays/dev-vm]", got)
	}
}

// TestAnsibleTargetsReadsEverythingOffTheRecord pins the bridge between
// truss's inventory and ansible's: address, login and groups all come from
// the host record, so there is no second place where "which machines are
// dev workstations" is written down and no second place to go stale.
func TestAnsibleTargetsReadsEverythingOffTheRecord(t *testing.T) {
	cluster := "hub"
	snap := inventory.Snapshot{Hosts: map[string]inventory.Host{
		"dev-agent": {
			Name: "dev-agent", Role: "dev-workstation",
			Access: &inventory.Access{Via: inventory.AccessAddress, Address: "dev-agent.invalid:22", User: "root"},
		},
		"vaultbox": {
			Name: "vaultbox", Role: "hub-node", Cluster: &cluster,
			// ⚠️ AN ADDRESS ON A TAILSCALE RECORD IS INVALID AND IS HERE ON
			// PURPOSE. inventory.Check refuses the pair, so a fixture that
			// omitted it made this test VACUOUS -- deleting the Via guard
			// below still assigned the empty string and every assertion
			// passed. The guard is belt-and-braces against a record that
			// reached this function anyway, and a test of it has to hand it
			// exactly that.
			Access: &inventory.Access{Via: inventory.AccessTailscale, Address: "vaultbox.invalid:22", User: "deploy"},
		},
	}}
	got := ansibleTargets(snap, []string{"dev-agent", "vaultbox"})
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	if got[0].Address != "dev-agent.invalid:22" || got[0].User != "root" {
		t.Fatalf("address host: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].Groups, []string{"dev-workstation"}) {
		t.Fatalf("groups came from somewhere other than role: %+v", got[0].Groups)
	}
	// ⚠️ A TAILNET HOST GETS NO ADDRESS, AND THAT IS THE ANSWER, NOT A GAP.
	// Its record states none -- truss refuses a record carrying both -- so
	// the name IS the address, which the renderer expresses by writing no
	// ansible_host and letting ansible connect to inventory_hostname.
	// Fully-qualifying it against a tailnet domain here would compile one
	// deployment's DNS into the engine.
	if got[1].Address != "" {
		t.Fatalf("a tailscale host was given an address: %+v", got[1])
	}
	if got[1].User != "deploy" {
		t.Fatalf("a tailscale host lost its login, so user and via were wrongly coupled: %+v", got[1])
	}
	if !reflect.DeepEqual(got[1].Groups, []string{"hub-node", "hub"}) {
		t.Fatalf("role and cluster did not both become groups: %+v", got[1].Groups)
	}
}
