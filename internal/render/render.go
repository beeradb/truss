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

	"github.com/beeradb/truss/internal/childproc"
)

// Runner drives the kustomize binary.
type Runner struct {
	// Bin is the kustomize executable. Required: there is no default, so a
	// caller that forgot to configure one is refused rather than silently
	// resolving whatever is first on PATH.
	//
	// ⚠️ THE VERSION IS PART OF THE DIGEST CONTRACT, AND NOTHING IN THIS
	// PACKAGE ENFORCES IT. Two renderers of different versions can emit
	// different bytes for one tree, which is a digest mismatch reported as
	// "the tree and the render disagree" -- true, but not the useful half of
	// the truth. What pins it is the image: kustomize-version at the repo
	// root feeds a required build-arg, the same shape OPENTOFU_VERSION uses.
	// A deployment whose CI renders with some other kustomize gets refusals
	// it will find hard to read, and no code here will tell it why.
	Bin string

	// Env is the EXACT environment the child gets. An empty slice means an
	// empty environment, never an inherited one -- exec.Cmd falls back to
	// os.Environ() when Env is nil, which would hand the renderer the
	// applier's own variables. plan.Runner solves this the same way and for
	// the same reason (runner.go:198-203).
	Env []string

	// Stderr receives everything kustomize writes there. Required, like Bin:
	// a render that failed for a reason nobody kept is a render nobody can
	// fix, and a nil io.Writer here does not panic -- exec.Cmd treats it as
	// "discard", so the transcript this whole package exists to preserve
	// would vanish silently instead of loudly. Build refuses it the same way
	// it refuses an empty Bin, rather than only documenting the expectation.
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
// the renderer FETCH A CHART FROM A REPOSITORY AT RENDER TIME, from a process
// that runs beside write credentials for four clouds. That reach is refused
// for renders the same way `tofu init -plugin-dir` refuses it for providers:
// resolve nothing from a registry while working. (⚠️ Truss itself bakes no
// provider mirror -- Dockerfile's own note says so, and the plugin cache
// belongs to the consumer -- so the parallel is the rule, not a mirror this
// image carries.) It also re-admits randAlphaNum, genCA and now,
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
	if r.Stderr == nil {
		return nil, fmt.Errorf("render: no stderr writer configured")
	}

	args := append(append([]string(nil), buildArgs...), dir)
	cmd := childproc.Command(ctx, r.Bin, args...)
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

// blackholeProxy is an address nothing listens on. It is appended to every
// render's environment so that any attempt to reach the network fails at
// connect, loudly, naming the URL it wanted.
//
// ⚠️ A RENDER READING THE NETWORK IS NOT HYPOTHETICAL AND THIS IS NOT BELT
// AND BRACES. `kustomize build` resolves a remote `resources:` entry over
// the network by default -- there is no flag to turn it off, and
// --load-restrictor governs local files rather than URLs. Measured on
// v5.7.1: a kustomization whose only resource is a GitHub URL renders
// cleanly, exit 0, with the content fetched at build time.
//
// That would quietly destroy the property this whole gate rests on. Both
// sides of a digest agree only because both read the same inputs; a remote
// base at a moving ref makes them read different ones, and the refusal would
// blame the tree when the world really had moved. Worse is the case where
// both sides fetch the SAME bytes -- then the gate passes and certifies
// manifests nobody reviewed. And it is a network reach from a process that
// runs beside credentials for four clouds.
//
// Measured with the proxy below: exit 1, zero bytes on stdout, and an error
// naming both the URL it wanted and the proxy that refused it. So the enforcement needs no YAML
// parser and no regex over a kustomization -- which matters, because
// AGENTS.md's rule is to gate on the field and never on rendered text, and a
// pattern written for one YAML style misses the other. internal/parity uses
// the same technique for the same reason.
//
// A unit that genuinely wants a remote base gets a refusal that names the
// URL, and the answer is the one charts get: vendor it into the reviewed
// diff, where somebody reads it.
// The host is a name that cannot resolve -- .invalid is reserved for exactly
// this by RFC 2606 -- rather than a loopback address. Two reasons, and the
// second is the better one: scripts/leakscan refuses an IP literal anywhere
// in this repository and is right to, since it cannot tell a black hole from
// somebody's cluster; and the name is carried verbatim into the error a
// blocked render produces, so the refusal explains itself to whoever reads
// the log instead of showing them a port nobody recognises.
const blackholeProxy = "http://truss-render-must-not-reach-the-network.invalid:1"

// explicitEnv returns a non-nil slice always, so exec.Cmd never falls back
// to the parent's environment -- and always ends with the proxy settings, so
// a caller cannot unset them by supplying their own. Later entries win in
// exec, which is what makes appending here an override rather than a
// suggestion. NO_PROXY is emptied for the same reason: left populated, it is
// the one variable that would wave a host straight past this.
func (r Runner) explicitEnv() []string {
	env := []string{}
	if r.Env != nil {
		env = append(env, r.Env...)
	}
	return append(env,
		"HTTP_PROXY="+blackholeProxy, "http_proxy="+blackholeProxy,
		"HTTPS_PROXY="+blackholeProxy, "https_proxy="+blackholeProxy,
		"ALL_PROXY="+blackholeProxy, "all_proxy="+blackholeProxy,
		"NO_PROXY=", "no_proxy=",
	)
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
