package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/render"
)

type fakeRender struct {
	out []byte
	err error

	// calls records every dir Build was actually given, in order. A pointer
	// field rather than a slice: fakeRender is passed by value throughout
	// this file (the same shape as the read-only fakes above it), and a
	// pointer is what lets every copy still write into the one caller-owned
	// slice -- the model is fakeTofu.Apply's appliedDirs in
	// testsupport_test.go, which exists for the identical reason: whether a
	// call happened at all, and with what argument, is the one thing a test
	// asserting a refusal or a pass-through cannot otherwise see.
	calls *[]string
}

func (f fakeRender) Build(ctx context.Context, dir string) ([]byte, error) {
	if f.calls != nil {
		*f.calls = append(*f.calls, dir)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func TestRenderUnitsForPicksOnlyRenderKinds(t *testing.T) {
	got := renderUnitsFor([]string{
		"projects/wren/main.tf",
		"deliveries/beta/web/kustomization.yaml",
		"baselines/prod/kustomization.yaml",
		"ansible/plays/k3s/play.yml",
		"credentials/cf.tf",
	}, nil)
	want := []string{"baselines/prod", "deliveries/beta/web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderUnitsFor = %v, want %v", got, want)
	}
}

// TestRenderUnitsForRerendersEveryUnitOnASharedInput is the shared-input
// branch of TouchedUnits, seen from renderUnitsFor. Every other test of this
// function passes nil for treeUnits, so a mutation that dropped treeUnits at
// the call site -- passing nil to TouchedUnits regardless of what this
// function itself was given -- would still pass every one of them. A shared
// input (here .kustomize-version, one of the paths unitSharedInput matches)
// must re-render every render unit that exists in the tree, not only the
// ones whose own files changed: the doc on renderUnitsFor calls that
// over-rendering an accepted cost rather than an oversight, because both
// sides of the digest gate derive the set with this same function.
func TestRenderUnitsForRerendersEveryUnitOnASharedInput(t *testing.T) {
	treeUnits := []string{"baselines/prod", "deliveries/beta/web", "deliveries/gamma/api"}
	got := renderUnitsFor([]string{".kustomize-version"}, treeUnits)
	want := []string{"baselines/prod", "deliveries/beta/web", "deliveries/gamma/api"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderUnitsFor(shared input, %v) = %v, want every unit in the tree %v", treeUnits, got, want)
	}
}

// TestRenderEnvCarriesNoCredential is the property the whole delivery gate
// rests on, checked directly rather than only inferred from a passing render:
// renderEnv's own doc says a render reads the tree and nothing else, which is
// what lets CI and the applier compute identical bytes for one unit. The
// comparison is exact -- not "contains PATH" and "contains HOME" -- because a
// mutation that appended a third, credential-bearing entry would satisfy
// either of those and still leak it into every kustomize child process this
// pass starts.
func TestRenderEnvCarriesNoCredential(t *testing.T) {
	d := applyDeps{PATH: "/usr/bin", HOME: "/root"}
	got := renderEnv(d)
	want := []string{"PATH=/usr/bin", "HOME=/root"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderEnv = %v, want exactly %v and nothing else", got, want)
	}
}

// TestNewRenderFactoryResolvesTheConfiguredBinary pins that the runner the
// pass actually builds carries kustomizeBin's answer. newRenderFactory's own
// doc says the applier and cmdRenderDigest (CI) MUST agree on which
// kustomize renders a tree, for the same reason internal/plan/digest.go and
// the consumer's jq must agree: when the two sides of a digest gate disagree
// about the tool, every apply is refused, or worse, both silently render
// different bytes from different kustomize versions.
func TestNewRenderFactoryResolvesTheConfiguredBinary(t *testing.T) {
	getenv := func(name string) string {
		if name == "KUSTOMIZE_BIN" {
			return "/opt/kustomize-fixture"
		}
		return ""
	}
	factory := newRenderFactory(getenv, io.Discard)
	runner := factory([]string{"PATH=/usr/bin"})
	rr, ok := runner.(render.Runner)
	if !ok {
		t.Fatalf("newRenderFactory returned %T, want render.Runner", runner)
	}
	if rr.Bin != "/opt/kustomize-fixture" {
		t.Errorf("Bin = %q, want the KUSTOMIZE_BIN override newRenderFactory was given", rr.Bin)
	}
}

// renderDeps builds a pass fixture whose git reports whether a unit exists.
func renderDeps(t *testing.T, hasDir bool) (applyDeps, *fakeLedger) {
	t.Helper()
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	git := &fakeGit{HasDirFn: func(string) bool { return hasDir }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)
	return deps, fl
}

// TestAnAbsentRenderUnitIsAPruneNotARefusal is where the kinds deliberately
// diverge. applyOneRoot refuses a root missing from the tree, because an
// OpenTofu root's absence can mean state holding live resources nobody
// manages. A delivery unit holds no state: its absence means the commit
// deleted it, and the reconciler removing what it applied IS the intended
// operation. Refusing here would make retiring a workload impossible without
// an operator override.
func TestAnAbsentRenderUnitIsAPruneNotARefusal(t *testing.T) {
	deps, _ := renderDeps(t, false)
	digest, reason := renderOneUnit(context.Background(), deps, fakeRender{err: errors.New("must not be called")},
		"headsha1", "deliveries/beta/web")
	if reason != "" {
		t.Fatalf("reason = %q, want none: a deleted delivery unit is a prune", reason)
	}
	if digest != "" {
		t.Errorf("digest = %q, want empty for a unit that is gone", digest)
	}
}

// TestRenderOneUnitPassesTheUnitsOwnDirectoryAndPathToHasDir catches two
// mutations that survive the rest of the suite because every other fixture
// ignores the argument it is given.
//
// (a) unitDir dropping "/"+unit renders the process's own working directory
// instead of the unit's -- and because both sides of the digest call the
// same Build, both would render the same wrong tree, agree, and prove
// nothing.
//
// (b) HasDir asked about a fixed "deliveries" rather than the unit itself
// FAILS OPEN: a wrong answer for "does deliveries/ exist" is read as "this
// unit is gone, prune it" and skips the render and the digest gate entirely
// -- see renderOneUnit's own comment on why an absent unit is a prune, not a
// refusal.
func TestRenderOneUnitPassesTheUnitsOwnDirectoryAndPathToHasDir(t *testing.T) {
	forgeFake := &fakeForge{ProtectionResult: compliantGatesProtection()}
	var hasDirArg string
	git := &fakeGit{HasDirFn: func(root string) bool {
		hasDirArg = root
		return true
	}}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)

	out := []byte("kind: Service\n")
	key := deps.Journal.Layout.DigestKey("headsha1", "deliveries/beta/web")
	fl.put(key, []byte(render.Digest(out)))

	var dirsSeen []string
	_, reason := renderOneUnit(context.Background(), deps, fakeRender{out: out, calls: &dirsSeen},
		"headsha1", "deliveries/beta/web")
	if reason != "" {
		t.Fatalf("reason = %q, want none", reason)
	}
	if hasDirArg != "deliveries/beta/web" {
		t.Errorf("HasDir was asked about %q, want the unit itself", hasDirArg)
	}
	want := deps.Cfg.Workdir + "/deliveries/beta/web"
	if len(dirsSeen) != 1 || dirsSeen[0] != want {
		t.Errorf("Build was given %v, want exactly [%q]", dirsSeen, want)
	}
}

func TestARenderMatchingItsApprovedDigestPasses(t *testing.T) {
	deps, fl := renderDeps(t, true)
	out := []byte("kind: Service\n")
	key := deps.Journal.Layout.DigestKey("headsha1", "deliveries/beta/web")
	fl.put(key, []byte(render.Digest(out)))

	digest, reason := renderOneUnit(context.Background(), deps, fakeRender{out: out},
		"headsha1", "deliveries/beta/web")
	if reason != "" {
		t.Fatalf("reason = %q, want none", reason)
	}
	if digest != render.Digest(out) {
		t.Errorf("digest = %q, want %q", digest, render.Digest(out))
	}
}

func TestARenderThatDoesNotMatchIsRefused(t *testing.T) {
	deps, fl := renderDeps(t, true)
	key := deps.Journal.Layout.DigestKey("headsha1", "deliveries/beta/web")
	fl.put(key, []byte(render.Digest([]byte("what CI rendered\n"))))

	_, reason := renderOneUnit(context.Background(), deps, fakeRender{out: []byte("something else\n")},
		"headsha1", "deliveries/beta/web")
	if reason == "" {
		t.Fatal("a render that does not match the approved digest was accepted")
	}
	if !strings.Contains(reason, "the tree and the render disagree") {
		t.Errorf("reason = %q, want the render-specific cause", reason)
	}
	// A render is a function of the tree alone, so the live world cannot be
	// what differed; saying it moved would send an operator to look at their
	// infrastructure for a fault that is in their repository.
	if strings.Contains(reason, "the world moved") {
		t.Errorf("reason = %q, want it not to blame the world", reason)
	}
}

// TestARenderNobodyFiledADigestForIsRefused: a check that passes when its own
// evidence is absent is not a check.
func TestARenderNobodyFiledADigestForIsRefused(t *testing.T) {
	deps, _ := renderDeps(t, true)
	_, reason := renderOneUnit(context.Background(), deps, fakeRender{out: []byte("x\n")},
		"headsha1", "deliveries/beta/web")
	if reason == "" {
		t.Fatal("a render with no recorded digest was accepted")
	}
	if !strings.Contains(reason, "nobody reviewed") {
		t.Errorf("reason = %q, want the unreviewed message", reason)
	}
}

func TestARenderThatFailsToBuildIsRefused(t *testing.T) {
	deps, _ := renderDeps(t, true)
	_, reason := renderOneUnit(context.Background(), deps, fakeRender{err: errors.New("exit status 1")},
		"headsha1", "deliveries/beta/web")
	if reason == "" {
		t.Fatal("a render that failed to build was accepted")
	}
	if !strings.Contains(reason, "deliveries/beta/web") {
		t.Errorf("reason = %q, want it to name the unit", reason)
	}
}

// unconfiguredRender is buildTestDeps' default. It never renders: a fixture
// that reaches the delivery path without declaring what the renderer
// produces has a hole in it, and an error naming that is worth more than
// empty bytes that hash to something.
type unconfiguredRender struct{}

func (unconfiguredRender) Build(ctx context.Context, dir string) ([]byte, error) {
	return nil, errors.New("this fixture did not configure a renderer; set deps.NewRender")
}

// TestACommitTouchingOnlyADeliveryIsRenderedNotNooped is the delivery gate
// seen from the pass. Before render units existed, such a commit derived no
// roots at all and was filed as a noop -- truss saying "I did nothing",
// which a reader could easily mistake for "nothing was done". Now it is
// rendered and checked against the digest CI filed.
//
// ⚠️ "NOT RECORDED AS A NOOP" IS NOT THE SAME CLAIM AS "IT WAS RENDERED", AND
// THIS TEST USED TO ONLY PROVE THE FIRST. Deleting the render loop out of
// runCommitLoop entirely leaves nothing to fail: the commit still has no
// roots, applied/<sha> is still written, and it still does not say "noop" --
// every assertion that existed here kept passing over a pass that skipped
// the render outright. calls is the fakeRender's own record of what
// actually ran, the same evidence fakeTofu.Apply's appliedDirs gives the
// tofu side (testsupport_test.go), and it is what makes "rendered" a checked
// fact rather than an inference from the absence of a noop record.
func TestACommitTouchingOnlyADeliveryIsRenderedNotNooped(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.RulesetsByBranch = map[string]gates.Rulesets{deliveryRef: protectedDeliveryRulesets()}
	git := &fakeGit{
		CommitsList:     []string{sha},
		ChangedByCommit: map[string][]string{sha: {"deliveries/beta/web/kustomization.yaml"}},
		TreeRenderUnitsByCommit: map[string][]string{
			sha: {"deliveries/beta/web"},
		},
		HasDirFn: func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)
	out := []byte("kind: Service\n")
	var calls []string
	deps.NewRender = func(env []string) renderRunner { return fakeRender{out: out, calls: &calls} }
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/web"), []byte(render.Digest(out)))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want none", result.failure)
	}
	if body, ok := fl.get("applied/" + sha); !ok {
		t.Error("no applied record for a commit whose delivery was rendered and matched")
	} else if strings.Contains(string(body), "noop") {
		t.Errorf("applied/%s = %s, want it not recorded as a noop", sha, body)
	}
	if len(calls) != 1 {
		t.Fatalf("render ran %d times, want exactly 1: the delivery must actually be rendered, not merely not filed as a noop", len(calls))
	}
}

// TestADeliveryWhoseRenderDoesNotMatchStopsTheQueue is the same path with the
// digest disagreeing: the pass must refuse the commit rather than advance.
func TestADeliveryWhoseRenderDoesNotMatchStopsTheQueue(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.RulesetsByBranch = map[string]gates.Rulesets{deliveryRef: protectedDeliveryRulesets()}
	git := &fakeGit{
		CommitsList:             []string{sha},
		ChangedByCommit:         map[string][]string{sha: {"deliveries/beta/web/kustomization.yaml"}},
		TreeRenderUnitsByCommit: map[string][]string{sha: {"deliveries/beta/web"}},
		HasDirFn:                func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)
	deps.NewRender = func(env []string) renderRunner { return fakeRender{out: []byte("ours\n")} }
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/web"),
		[]byte(render.Digest([]byte("what CI rendered\n"))))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure == "" {
		t.Fatal("a delivery whose render did not match was allowed through")
	}
	if !strings.Contains(result.failure, "the tree and the render disagree") {
		t.Errorf("failure = %q, want the render-specific cause", result.failure)
	}
	if got, ok := fl.get("head"); ok && strings.Contains(string(got), sha) {
		t.Errorf("head = %q, want it left at %s", got, head)
	}
}

// TestASecondRenderUnitThatDoesNotMatchStopsThePass is a commit touching two
// delivery units, the first matching its approved digest and the second not.
// The render loop in runCommitLoop (apply_cmd.go) ranges over every render
// unit the commit touches; mutating that to only the first
// (renderUnits[:1]) never reaches the second unit's mismatch, so the pass
// would see no reason to refuse and record the commit as applied. No
// existing test could catch that: every other one drives exactly one render
// unit, where [:1] and the full slice are the same slice.
func TestASecondRenderUnitThatDoesNotMatchStopsThePass(t *testing.T) {
	const sha = "commitsha5"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, head)
	git := &fakeGit{
		CommitsList: []string{sha},
		ChangedByCommit: map[string][]string{sha: {
			"deliveries/beta/api/kustomization.yaml",
			"deliveries/beta/web/kustomization.yaml",
		}},
		// renderUnitsFor sorts by path, so "deliveries/beta/api" is rendered
		// before "deliveries/beta/web" regardless of this listing's order.
		TreeRenderUnitsByCommit: map[string][]string{
			sha: {"deliveries/beta/web", "deliveries/beta/api"},
		},
		HasDirFn: func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)

	outAPI := []byte("api\n")
	outWeb := []byte("web\n")
	var order []string
	deps.NewRender = func(env []string) renderRunner {
		if len(order) == 0 {
			order = append(order, "first-call")
			return fakeRender{out: outAPI}
		}
		return fakeRender{out: outWeb}
	}

	// The first unit's render matches what CI filed; the second's does not.
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/api"), []byte(render.Digest(outAPI)))
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/web"),
		[]byte(render.Digest([]byte("what CI rendered\n"))))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)

	if result.failure == "" {
		t.Fatal("a commit whose second render unit did not match its approved digest was allowed through")
	}
	if !strings.Contains(result.failure, "the tree and the render disagree") {
		t.Errorf("failure = %q, want the render-specific cause", result.failure)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Error("applied/" + sha + " was written for a commit whose second unit never matched")
	}
}

// deliveryPass builds a commit that touches one delivery unit whose render
// matches, so the only variable left is the delivery ref's protection.
func deliveryPass(t *testing.T, rulesets map[string]gates.Rulesets) (applyDeps, *fakeLedger, *fakeGit, string, string) {
	t.Helper()
	const sha, head = "commitsha3", "headsha1"
	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.RulesetsByBranch = rulesets
	git := &fakeGit{
		CommitsList:             []string{sha},
		ChangedByCommit:         map[string][]string{sha: {"deliveries/beta/web/kustomization.yaml"}},
		TreeRenderUnitsByCommit: map[string][]string{sha: {"deliveries/beta/web"}, head: {"deliveries/beta/web"}},
		HasDirFn:                func(string) bool { return true },
	}
	deps, fl, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return &fakeTofu{} })
	out := []byte("kind: Service\n")
	deps.NewRender = func([]string) renderRunner { return fakeRender{out: out} }
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/web"), []byte(render.Digest(out)))
	return deps, fl, git, sha, head
}

func TestAPassPublishesTheDeliveryRefWhenTheQueueAdvances(t *testing.T) {
	deps, _, git, sha, head := deliveryPass(t, map[string]gates.Rulesets{deliveryRef: protectedDeliveryRulesets()})
	git.TreeRenderUnitsByCommit[sha] = []string{"deliveries/beta/web"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, head); result.failure != "" {
		t.Fatalf("result.failure = %q, want none", result.failure)
	}
	want := deliveryRef + "=" + sha
	if len(git.PushedRefs) != 1 || git.PushedRefs[0] != want {
		t.Fatalf("PushedRefs = %v, want exactly [%s]", git.PushedRefs, want)
	}
}

// TestAPassRefusesToPublishOntoAnUnprotectedRef is the reason the gate runs
// before the push. A ref a reconciler applies from is a path to production,
// and one nothing protects is not a weaker gate -- it is a path nobody is
// watching. Discovering that after publishing would be discovering it late.
func TestAPassRefusesToPublishOntoAnUnprotectedRef(t *testing.T) {
	// No entry for the delivery ref: the forge reports nothing protects it.
	deps, _, git, _, head := deliveryPass(t, map[string]gates.Rulesets{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)
	if result.failure == "" {
		t.Fatal("the pass published onto a ref nothing protects")
	}
	if !strings.Contains(result.failure, "no ruleset applies") {
		t.Errorf("failure = %q, want it to say nothing protects the ref", result.failure)
	}
	if len(git.PushedRefs) != 0 {
		t.Errorf("PushedRefs = %v, want nothing pushed", git.PushedRefs)
	}
}

// TestAPassWithNoDeliveryUnitsNeverTouchesTheRef: requiring a protected ref
// from a deployment that has no manifests at all would make delivery a tax on
// people who never asked for it. The tree decides, not a flag.
func TestAPassWithNoDeliveryUnitsNeverTouchesTheRef(t *testing.T) {
	const sha, head = "commitsha3", "headsha1"
	forgeFake := compliantCommitGate("alice", sha, head)
	git := &fakeGit{
		CommitsList:     []string{sha},
		ChangedByCommit: map[string][]string{sha: {"docs/README.md"}},
		HasDirFn:        func(string) bool { return true },
	}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return &fakeTofu{} })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, head); result.failure != "" {
		t.Fatalf("result.failure = %q, want none: a tree with no delivery units owes nothing to a ref", result.failure)
	}
	if len(git.PushedRefs) != 0 {
		t.Errorf("PushedRefs = %v, want nothing pushed", git.PushedRefs)
	}
}

// --- the delivery metrics --------------------------------------------------

// TestAPublishedDeliveryReportsItself is truss_delivery_published and
// truss_render_units/truss_render_refusals seen end to end: a clean publish
// says so, and says nothing refused it.
func TestAPublishedDeliveryReportsItself(t *testing.T) {
	g := newGateway(t)
	deps, _, git, sha, head := deliveryPass(t, map[string]gates.Rulesets{deliveryRef: protectedDeliveryRulesets()})
	git.TreeRenderUnitsByCommit[sha] = []string{"deliveries/beta/web"}
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, head); result.failure != "" {
		t.Fatalf("result.failure = %q, want none", result.failure)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_delivery_published"); got != "1" {
		t.Errorf("truss_delivery_published = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_delivery_ref_unprotected"); got != "0" {
		t.Errorf("truss_delivery_ref_unprotected = %s, want 0 -- nothing refused this publish", got)
	}
	if got := sampleValue(t, body, "truss_render_units"); got != "1" {
		t.Errorf("truss_render_units = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_render_refusals"); got != "0" {
		t.Errorf("truss_render_refusals = %s, want 0", got)
	}
}

// TestAnUnprotectedDeliveryRefReportsItself is the metric
// truss_delivery_ref_unprotected exists for: gated commits piling up with
// nothing telling a cluster about them. truss_delivery_published must stay
// 0 -- the pass refused, it did not publish.
func TestAnUnprotectedDeliveryRefReportsItself(t *testing.T) {
	g := newGateway(t)
	deps, _, git, _, head := deliveryPass(t, map[string]gates.Rulesets{})
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)
	if result.failure == "" {
		t.Fatal("the pass published onto a ref nothing protects")
	}
	if len(git.PushedRefs) != 0 {
		t.Errorf("PushedRefs = %v, want nothing pushed", git.PushedRefs)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_delivery_ref_unprotected"); got != "1" {
		t.Errorf("truss_delivery_ref_unprotected = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_delivery_published"); got != "0" {
		t.Errorf("truss_delivery_published = %s, want 0 -- refused, not published", got)
	}
}

// TestANoDeliveryUnitsPassDoesNotClaimTheRefIsUnprotected is the conflation
// truss_delivery_ref_unprotected exists to prevent. A deployment that never
// uses delivery never asks the forge about the ref at all, so it must not
// report the ref as unprotected -- that would tell somebody who never asked
// for the feature that gated commits are stuck, when the truth is there is
// nothing to deliver.
func TestANoDeliveryUnitsPassDoesNotClaimTheRefIsUnprotected(t *testing.T) {
	const sha, head = "commitsha3", "headsha1"
	g := newGateway(t)
	forgeFake := compliantCommitGate("alice", sha, head)
	git := &fakeGit{
		CommitsList:     []string{sha},
		ChangedByCommit: map[string][]string{sha: {"docs/README.md"}},
		HasDirFn:        func(string) bool { return true },
	}
	deps, _, _ := buildTestDeps(t, forgeFake, git, func([]string) tofuRunner { return &fakeTofu{} })
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, head); result.failure != "" {
		t.Fatalf("result.failure = %q, want none", result.failure)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_delivery_ref_unprotected"); got != "0" {
		t.Errorf("truss_delivery_ref_unprotected = %s, want 0: a tree with no delivery units never asked the forge about the ref", got)
	}
	if got := sampleValue(t, body, "truss_delivery_published"); got != "0" {
		t.Errorf("truss_delivery_published = %s, want 0: nothing to publish", got)
	}
}

// TestARenderRefusalIsCountedSeparatelyFromTheDigestGate is
// truss_render_refusals seen end to end: a mismatched render must move that
// series and must NOT move truss_digest_refusals, which is a different gate
// over a different artefact -- the OpenTofu plan, not the rendered manifest.
func TestARenderRefusalIsCountedSeparatelyFromTheDigestGate(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	g := newGateway(t)
	forgeFake := compliantCommitGate("alice", sha, head)
	forgeFake.RulesetsByBranch = map[string]gates.Rulesets{deliveryRef: protectedDeliveryRulesets()}
	git := &fakeGit{
		CommitsList:             []string{sha},
		ChangedByCommit:         map[string][]string{sha: {"deliveries/beta/web/kustomization.yaml"}},
		TreeRenderUnitsByCommit: map[string][]string{sha: {"deliveries/beta/web"}},
		HasDirFn:                func(string) bool { return true },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }

	deps, fl, _ := buildTestDeps(t, forgeFake, git, newTofu)
	deps.Cfg.MetricsPushURL = g.srv.URL
	deps.NewRender = func(env []string) renderRunner { return fakeRender{out: []byte("ours\n")} }
	fl.put(deps.Journal.Layout.DigestKey(head, "deliveries/beta/web"),
		[]byte(render.Digest([]byte("what CI rendered\n"))))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, head)
	if result.failure == "" {
		t.Fatal("a delivery whose render did not match was allowed through")
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_render_units"); got != "1" {
		t.Errorf("truss_render_units = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_render_refusals"); got != "1" {
		t.Errorf("truss_render_refusals = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_digest_refusals"); got != "0" {
		t.Errorf("truss_digest_refusals = %s, want 0: the plan gate never ran, only the render gate did", got)
	}
}
