package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/render"
)

type fakeRender struct {
	out []byte
	err error
}

func (f fakeRender) Build(ctx context.Context, dir string) ([]byte, error) {
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
func TestACommitTouchingOnlyADeliveryIsRenderedNotNooped(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, head)
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
	deps.NewRender = func(env []string) renderRunner { return fakeRender{out: out} }
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
}

// TestADeliveryWhoseRenderDoesNotMatchStopsTheQueue is the same path with the
// digest disagreeing: the pass must refuse the commit rather than advance.
func TestADeliveryWhoseRenderDoesNotMatchStopsTheQueue(t *testing.T) {
	const sha = "commitsha3"
	const head = "headsha1"

	forgeFake := compliantCommitGate("alice", sha, head)
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
