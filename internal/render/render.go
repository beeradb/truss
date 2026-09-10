// Package render turns a delivery unit's directory into the exact bytes a
// cluster will be asked to apply, and fingerprints them.
//
// It is the delivery-side counterpart of internal/plan, and it is much
// smaller for one reason: a render is a function of the tree alone. It needs
// no state, no credentials, no provider and no network, so the two sides of
// the digest gate -- a read-only CI job and the applier -- are computing over
// exactly the same inputs. internal/plan had to reproduce a jq pipeline
// byte-for-byte and filter out no-op entries, because two identities with
// different read permissions see different attribute values in one plan
// (digest.go:40-58). Nothing analogous can happen here: there is no identity
// involved, so there is nothing to canonicalise and the bytes are hashed as
// they are.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
)

// Runner drives the kustomize binary.
type Runner struct {
	// Bin is the kustomize executable. Required: there is no default, so a
	// caller that forgot to configure one is refused rather than silently
	// resolving whatever is first on PATH -- the version is part of the
	// digest contract, and "whichever kustomize this machine happens to
	// have" is exactly the drift the pinned .kustomize-version exists to
	// prevent.
	Bin string

	// Env is the EXACT environment the child gets. An empty slice means an
	// empty environment, never an inherited one -- exec.Cmd falls back to
	// os.Environ() when Env is nil, which would hand the renderer the
	// applier's own variables. plan.Runner solves this the same way and for
	// the same reason (runner.go:198-203).
	Env []string

	// Stderr receives everything kustomize writes there. Never nil in
	// production: a render that failed for a reason nobody kept is a render
	// nobody can fix.
	Stderr io.Writer
}

// ⚠️ --enable-helm IS NEVER PASSED, AND THAT IS THE WHOLE OF THE NO-HELM
// RULE.
//
// A kustomization carrying a helmCharts field is refused by kustomize
// itself when the flag is absent. Measured against kustomize v5.7.1: exit
// status 1, zero bytes on stdout, "must specify --enable-helm" on stderr.
// So the rule needs no YAML parser and no regex over the kustomization --
// which matters, because AGENTS.md's standing rule is to gate on the field
// and never on rendered text, and a pattern matching one YAML style would
// have missed the other.
//
// Refusing it is not squeamishness about Helm as a tool. --enable-helm makes
// the renderer FETCH A CHART FROM A REPOSITORY AT RENDER TIME, which is the
// network reach the baked provider mirror exists to prevent: a box holding
// write credentials for four clouds must not resolve anything from a package
// registry while it works. It also re-admits randAlphaNum, genCA and now,
// none of which can ever hash to the same bytes twice. Charts are inflated
// once, by a human, and the rendered manifests are committed.
var buildArgs = []string{"build"}

// Build renders dir and returns exactly the bytes kustomize wrote to stdout.
//
// Nothing kustomize prints to stderr reaches the returned bytes or the
// returned error's text. That is deliberate and it mirrors
// plan.wrapExecError: an error string here travels into the ledger and into
// a chat message, and a renderer's transcript can carry file paths and URLs
// from a tree this process does not control.
func (r Runner) Build(ctx context.Context, dir string) ([]byte, error) {
	if r.Bin == "" {
		return nil, fmt.Errorf("render: no kustomize binary configured")
	}
	if dir == "" {
		return nil, fmt.Errorf("render: no directory given")
	}

	args := append(append([]string(nil), buildArgs...), dir)
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = r.explicitEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = r.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("render: kustomize build %s: %v", dir, exitOnly(err))
	}

	// An empty render is refused rather than hashed. Zero bytes is what a
	// build that produced nothing looks like AND what several kinds of
	// silent misconfiguration look like -- an empty resources list, a
	// directory that is not a kustomization at all -- and the digest of
	// nothing agrees with the digest of every other nothing. A gate whose
	// two sides can agree on emptiness is a gate that passes on garbage.
	if out.Len() == 0 {
		return nil, fmt.Errorf("render: kustomize build %s produced no output", dir)
	}
	return out.Bytes(), nil
}

// BuildStable renders twice and refuses if the two renders differ.
//
// It is for the witness side, not the applier: CI runs it so that a unit
// with a non-deterministic input is caught on the day it is added rather
// than on the day it wedges the queue. The applier renders once and compares
// against the recorded digest, because a second render there would only
// re-answer a question the gate is already asking.
func (r Runner) BuildStable(ctx context.Context, dir string) ([]byte, error) {
	first, err := r.Build(ctx, dir)
	if err != nil {
		return nil, err
	}
	second, err := r.Build(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(first, second) {
		return nil, fmt.Errorf("render: %s does not render the same twice: it has a non-deterministic input, and no digest can ever match it", dir)
	}
	return first, nil
}

// Digest is the fingerprint the gate compares: SHA-256 of the rendered
// bytes, as 64 lowercase hex characters.
//
// ⚠️ THESE BYTES ARE FROZEN THE DAY THE FIRST DIGEST IS RECORDED AGAINST
// THEM. There is no canonicalisation step to change later, which is the
// point -- internal/plan/digest.go carries 700 lines and a differential
// fuzzer precisely because it has one. Hashing the renderer's output
// verbatim means the only thing that can move these bytes is the pinned
// kustomize version, and that pin lives in the reviewed tree.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// explicitEnv returns a non-nil slice always, so exec.Cmd never falls back
// to the parent's environment.
func (r Runner) explicitEnv() []string {
	if r.Env == nil {
		return []string{}
	}
	return r.Env
}

// exitOnly reduces an exec error to its status. The transcript is already on
// Stderr, where an operator reads it; repeating it in the error would put it
// in the ledger and the alert as well.
func exitOnly(err error) error {
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return fmt.Errorf("exit status %d", ee.ExitCode())
	}
	return err
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
