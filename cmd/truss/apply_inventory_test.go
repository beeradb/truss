package main

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// ⚠️ THE INVENTORY GATE WAS BUILT AND TESTED IN internal/inventory AND
// NOTHING IN cmd/truss CALLED IT. inventory.Load, inventory.Check and
// inventory.CheckMoves had their own thorough unit tests and zero coverage
// of the one thing that makes them matter: that runCommitLoop actually asks
// them, at every commit, before anything is planned or rendered. This file
// is that wiring test, the same shape apply_digest_gate_test.go is for the
// digest gate.
//
// Every fixture tree below is deliberately minimal and copies its shape
// from internal/inventory/load_test.go's validTree() -- the smallest
// inventory that package's own Load/Check pair accepts -- rather than
// reusing that function directly, which is unexported in a different
// package.

// invValidTree is internal/inventory/load_test.go's validTree(), copied
// verbatim: one host, one cluster, one project, one stateless environment
// with a matching delivery unit. Check accepts it with no problems.
func invValidTree() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"inventory/hosts/alpha.json": f(`{
			"schema":"truss.host/v1","name":"alpha","kind":"vm","role":"k8s-node",
			"provisioned_by":"hosts/alpha","config":"ansible/alpha",
			"tailnet_tags":["k8s"],"cluster":"prod","frozen":false,"decommissioned":false}`),
		"inventory/clusters/prod.json": f(`{
			"schema":"truss.cluster/v1","name":"prod","distribution":"k3s",
			"hosts":["alpha"],"baseline":"baselines/prod","capabilities":["ingress"],
			"kubeconfig_item":"prod-kubeconfig","has_ha_vault":true}`),
		"inventory/projects/wren.json": f(`{
			"schema":"truss.project/v1","name":"wren","environments":["prod"]}`),
		"inventory/environments/wren/prod.json": f(`{
			"schema":"truss.environment/v1","project":"wren","name":"prod",
			"shape":"kubernetes","placement":{"cluster":"prod","namespace":"web"},
			"requires":["ingress"],"vault":{"mount":"secret","prefix":"wren/prod"},
			"frozen":false,"stateful":false}`),
		"deliveries/prod/web/kustomization.yaml": f("kind: Kustomization\n"),
	}
}

// invDanglingClusterTree carries one environment whose placement.cluster
// names "ghost", a cluster with no inventory/clusters/ghost.json record at
// all -- the dangling reference inventory.Check exists to catch.
func invDanglingClusterTree() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"inventory/projects/wren.json": f(`{"schema":"truss.project/v1","name":"wren","environments":["prod"]}`),
		"inventory/environments/wren/prod.json": f(`{"schema":"truss.environment/v1","project":"wren","name":"prod",` +
			`"shape":"kubernetes","placement":{"cluster":"ghost","namespace":"web"},` +
			`"requires":[],"vault":{"mount":"secret","prefix":"wren/prod"},"frozen":false,"stateful":false}`),
	}
}

// invStatefulTree builds a minimal, otherwise-consistent inventory placing
// environment wren/prod on cluster, with the given Stateful value and a
// matching delivery unit -- the fixture two commits of the same test share,
// one per snapshot CheckMoves compares.
func invStatefulTree(cluster string, stateful bool) fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	statefulLit := "false"
	if stateful {
		statefulLit = "true"
	}
	return fstest.MapFS{
		"inventory/clusters/" + cluster + ".json": f(`{"schema":"truss.cluster/v1","name":"` + cluster + `",` +
			`"distribution":"k3s","hosts":[],"baseline":"baselines/` + cluster + `","capabilities":["ingress"],` +
			`"kubeconfig_item":"` + cluster + `-kubeconfig","has_ha_vault":true}`),
		"inventory/projects/wren.json": f(`{"schema":"truss.project/v1","name":"wren","environments":["prod"]}`),
		"inventory/environments/wren/prod.json": f(`{"schema":"truss.environment/v1","project":"wren","name":"prod",` +
			`"shape":"kubernetes","placement":{"cluster":"` + cluster + `","namespace":"web"},` +
			`"requires":[],"vault":{"mount":"secret","prefix":"wren/prod"},"frozen":false,"stateful":` + statefulLit + `}`),
		"deliveries/" + cluster + "/web/kustomization.yaml": f("kind: Kustomization\n"),
	}
}

// invApplyDeps assembles applyDeps over git and tofu, with a commit gate
// that passes for sha (approved and merged at head).
func invApplyDeps(t *testing.T, sha, head string, git gitDriver, tofu tofuRunner) (applyDeps, *fakeLedger) {
	t.Helper()
	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.ProtectionResult = compliantGatesProtection()
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return tofu })
	return deps, fl
}

func runInvPass(t *testing.T, deps applyDeps, last string) applyResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runApplyPass(ctx, deps, last)
}

// TestInventoryGateAppliesAConsistentInventory: the ordinary case. A commit
// touching both a root and a consistent inventory must apply normally --
// this is the guard that a correct inventory is never, itself, a reason to
// refuse.
func TestInventoryGateAppliesAConsistentInventory(t *testing.T) {
	const sha = "invconsistent"
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {"platform/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "platform" },
		TreeFSBySha:       map[string]fs.FS{sha: invValidTree()},
	}
	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, fl := invApplyDeps(t, sha, sha, git, tofu)
	fl.put("digests/"+sha+"/platform.digest", []byte(ourDigest(t)))

	result := runInvPass(t, deps, "startsha")
	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- the inventory is consistent", result.failure)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Errorf("applied/%s was not written", sha)
	}
}

// TestInventoryGateRefusesADanglingClusterReference: an environment placing
// itself on a cluster with no record is refused, the refusal names the
// environment's own file, and HEAD does not advance past it.
func TestInventoryGateRefusesADanglingClusterReference(t *testing.T) {
	const sha = "invdangling"
	git := &fakeGit{
		CommitsList: []string{sha},
		TreeFSBySha: map[string]fs.FS{sha: invDanglingClusterTree()},
	}
	tofu := &fakeTofu{}
	deps, fl := invApplyDeps(t, sha, sha, git, tofu)

	result := runInvPass(t, deps, "startsha")
	if result.failure == "" {
		t.Fatal("result.failure is empty, want a refusal -- the environment names a cluster that does not exist")
	}
	if !strings.Contains(result.failure, "inventory/environments/wren/prod.json") {
		t.Errorf("refusal %q does not name the environment file", result.failure)
	}
	if !strings.Contains(result.failure, "ghost") {
		t.Errorf("refusal %q does not name the dangling cluster", result.failure)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Errorf("applied/%s was written for a refused commit", sha)
	}
	if _, ok := fl.get("failed/" + sha); !ok {
		t.Errorf("failed/%s was not written for a refused commit", sha)
	}
	if headBytes, ok := fl.get("head"); ok {
		t.Errorf("head was advanced to %q, want it left unmoved", headBytes)
	}
}

// TestInventoryGateRefusesAStatefulMoveBetweenClusters: a stateful
// environment's placement.cluster changing between a commit and its parent
// is refused, and the message names both clusters -- the data-loss move
// inventory.CheckMoves exists to stop.
func TestInventoryGateRefusesAStatefulMoveBetweenClusters(t *testing.T) {
	const head = "invmovehead"
	const parent = "invmoveparent"
	git := &fakeGit{
		CommitsList: []string{head},
		TreeFSBySha: map[string]fs.FS{
			head:   invStatefulTree("backup", true),
			parent: invStatefulTree("prod", true),
		},
		ParentBySha: map[string]string{head: parent},
	}
	tofu := &fakeTofu{}
	deps, fl := invApplyDeps(t, head, head, git, tofu)

	result := runInvPass(t, deps, "startsha")
	if result.failure == "" {
		t.Fatal("result.failure is empty, want a refusal -- a stateful environment moved clusters")
	}
	if !strings.Contains(result.failure, "prod") || !strings.Contains(result.failure, "backup") {
		t.Errorf("refusal %q does not name both clusters", result.failure)
	}
	if _, ok := fl.get("applied/" + head); ok {
		t.Errorf("applied/%s was written for a refused commit", head)
	}
}

// TestInventoryGateAppliesAStatelessMoveBetweenClusters: the same move,
// with stateful:false, is not CheckMoves' concern -- moving a stateless
// workload is an ordinary reviewable change, and it applies.
func TestInventoryGateAppliesAStatelessMoveBetweenClusters(t *testing.T) {
	const head = "invstatelesshead"
	const parent = "invstatelessparent"
	git := &fakeGit{
		CommitsList: []string{head},
		TreeFSBySha: map[string]fs.FS{
			head:   invStatefulTree("backup", false),
			parent: invStatefulTree("prod", true),
		},
		ParentBySha: map[string]string{head: parent},
	}
	tofu := &fakeTofu{}
	deps, fl := invApplyDeps(t, head, head, git, tofu)

	result := runInvPass(t, deps, "startsha")
	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- the moved environment is not stateful", result.failure)
	}
	if _, ok := fl.get("applied/" + head); !ok {
		t.Errorf("applied/%s was not written", head)
	}
}

// TestInventoryGateSkipsATreeWithNoInventoryDirectory is the
// parity-protecting case: a deployment that has never adopted the
// inventory must not have every one of its commits refused for the
// omission. internal/parity's 43 recorded scenarios carry no inventory/
// directory at all, and this is the behaviour that keeps them green.
func TestInventoryGateSkipsATreeWithNoInventoryDirectory(t *testing.T) {
	const sha = "invnodir"
	git := &fakeGit{
		CommitsList:       []string{sha},
		ChangedByCommit:   map[string][]string{sha: {"platform/main.tf"}},
		TreeRootsByCommit: map[string][]string{sha: nil},
		HasDirFn:          func(root string) bool { return root == "platform" },
		// TreeFSBySha deliberately has no entry for sha: fakeGit's default is
		// an empty fstest.MapFS, exactly what a deployment that has never
		// adopted the inventory looks like.
	}
	tofu := &fakeTofu{PlanDetailedChanged: true}
	deps, fl := invApplyDeps(t, sha, sha, git, tofu)
	fl.put("digests/"+sha+"/platform.digest", []byte(ourDigest(t)))

	result := runInvPass(t, deps, "startsha")
	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a deployment with no inventory/ must not be refused for lacking one", result.failure)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Errorf("applied/%s was not written", sha)
	}
}

// TestInventoryGateSkipsCheckMovesWithNoParent: a commit with no recorded
// parent (git.Parent's ok=false) still runs Check on the head, but
// CheckMoves has nothing to compare against and must not be attempted.
func TestInventoryGateSkipsCheckMovesWithNoParent(t *testing.T) {
	const sha = "invnoparent"
	git := &fakeGit{
		CommitsList: []string{sha},
		TreeFSBySha: map[string]fs.FS{sha: invStatefulTree("prod", true)},
		// ParentBySha deliberately unset: Parent(sha) returns ok=false.
	}
	tofu := &fakeTofu{}
	deps, fl := invApplyDeps(t, sha, sha, git, tofu)

	result := runInvPass(t, deps, "startsha")
	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- no parent means CheckMoves is skipped, not refused", result.failure)
	}
	if _, ok := fl.get("applied/" + sha); !ok {
		t.Errorf("applied/%s was not written", sha)
	}
}
