package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/render"
	"github.com/beeradb/truss/internal/repo"
)

// renderRunner is the subset of render.Runner the pass uses, as a local
// interface so a test can drive the delivery path without a kustomize
// binary -- the same reason gitDriver and tofuRunner exist.
type renderRunner interface {
	Build(ctx context.Context, dir string) ([]byte, error)
}

// renderFactory builds a runner for one pass. It mirrors tofuFactory.
type renderFactory func(env []string) renderRunner

// renderUnitsFor derives the render units a commit touches.
//
// ⚠️ treeUnits HERE IS THE RENDER UNITS OF THE TREE, NOT EVERY UNIT. It is
// fed from gitDriver.TreeRenderUnits, and only the render half of the result
// is read, so the shared-input branch of TouchedUnits -- which returns
// everything in the tree -- returns exactly the render units that exist.
// The tofu half of a commit is still derived by TouchedRoots, untouched,
// because internal/parity compares that function against recordings of the
// bash.
//
// A shared input therefore re-renders every unit rather than only the ones
// whose own files changed, and that is accepted cost rather than an
// oversight: CI derives the units with this same function, so both sides
// agree on the set and the extra renders simply cost time. A second,
// kind-aware rule would have to be implemented identically in two places,
// which is the shape of drift this repository has already paid for once.
func renderUnitsFor(changedFiles, treeUnits []string) []string {
	var out []string
	for _, u := range repo.TouchedUnits(changedFiles, treeUnits) {
		if u.Kind == repo.KindRender {
			out = append(out, u.Path)
		}
	}
	return out
}

// renderOneUnit renders one delivery unit and refuses unless the bytes hash
// to what CI filed for this commit. It applies nothing: a reconciler does
// that, from a ref the applier advances only once every unit of the commit
// has passed here.
//
// The empty reason means "this unit is settled", which includes the case
// where it no longer exists -- see below.
func renderOneUnit(ctx context.Context, d applyDeps, r renderRunner, headSHA, unit string) (digest string, reason string) {
	// ⚠️ AN ABSENT RENDER UNIT IS A PRUNE, NOT A REFUSAL, AND THIS IS THE
	// ONE PLACE THE KINDS DELIBERATELY DIVERGE.
	//
	// applyOneRoot refuses a root that is not in the tree ("root %s does not
	// exist at %s"), and it is right to: an OpenTofu root's absence can mean
	// state holding live resources nobody is managing any more, which is a
	// question for a human. A delivery unit holds no state. Its absence at
	// this commit means the commit deleted it, and deleting it IS the
	// intended operation -- the reconciler removes what it previously
	// applied. Refusing here would make the ordinary act of retiring a
	// workload impossible without an operator override.
	if !d.Git.HasDir(unit) {
		d.logf("render: %s is gone at %s; the reconciler prunes what it applied", unit, headSHA)
		return "", ""
	}

	unitDir := d.Cfg.Workdir + "/" + unit
	out, err := r.Build(ctx, unitDir)
	if err != nil {
		return "", fmt.Sprintf("could not render %s at %s: %v", unit, headSHA, err)
	}

	mine := render.Digest(out)
	approved, err := d.Journal.ApprovedDigest(ctx, headSHA, unit)
	approvedFound := true
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			approvedFound = false
		} else {
			return "", fmt.Sprintf("could not read the approved render digest for %s at %s: %v", unit, headSHA, err)
		}
	}

	key := d.Journal.Layout.DigestKey(headSHA, unit)
	if problems := gates.CheckRenderDigest(unit, headSHA, key, mine, approved, approvedFound); len(problems) > 0 {
		return "", strings.Join(problems, "; ")
	}

	d.logf("render for %s matches the one approved at %s", unit, headSHA)
	return mine, ""
}

// renderEnv is the whole environment a render gets: PATH so the binary can
// be found, HOME because some tools write a cache there, and nothing else.
//
// ⚠️ NO CREDENTIAL IS PASSED, DELIBERATELY, AND THAT IS THE PROPERTY THE
// DELIVERY GATE RESTS ON. A render reads the tree and nothing else, which is
// what lets a read-only CI job and the applier compute the same bytes. The
// moment a render could read a credential it could also produce output that
// depends on who ran it, and the two sides would stop agreeing -- the same
// failure internal/plan had to filter no-op entries to escape.
func renderEnv(d applyDeps) []string {
	return []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
}

// kustomizeBin resolves the renderer, optional-with-default in the shape of
// SECRETS_DIR and TF_PLUGIN_DIR -- but deliberately not a config.Config
// field, because config_test.go pins the exact count of those and this knob
// is not the applier's to grow.
//
// One definition with two callers: cmdRenderDigest resolves it for CI and
// cmdApply for the pass. The two MUST agree, for the same reason
// internal/plan/digest.go and the consumer's jq must -- when the two sides
// of a digest gate disagree about the tool, every apply is refused.
func kustomizeBin(getenv func(string) string) string {
	if bin := getenv("KUSTOMIZE_BIN"); bin != "" {
		return bin
	}
	return defaultKustomizeBin
}

// newRenderFactory builds the pass's render factory. It lives here rather
// than inline in cmdApply so apply_cmd.go needs no render import and the
// renderer's configuration sits beside the code that uses it.
func newRenderFactory(getenv func(string) string, stderr io.Writer) renderFactory {
	bin := kustomizeBin(getenv)
	return func(env []string) renderRunner {
		return render.Runner{Bin: bin, Stderr: stderr, Env: env}
	}
}
