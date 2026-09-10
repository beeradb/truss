package main

import (
	"context"
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/render"
)

// defaultKustomizeBin is what CI gets when it does not set KUSTOMIZE_BIN, the
// same optional-with-default shape as SECRETS_DIR and TF_PLUGIN_DIR in
// internal/config -- but this value is deliberately NOT a Config field.
// config.Load is the applier's config, and config_test.go pins the exact
// count of its optional-with-default variables; render-digest runs only in
// CI, never inside the applier, so its one knob is read here directly.
const defaultKustomizeBin = "/usr/local/bin/kustomize"

// cmdRenderDigest is the CI witness side of the delivery gate: render dir
// with kustomize and print the digest CI files under the plan-digest prefix
// for the applier to compare against later.
//
// It calls BuildStable, not Build. This is the one place a non-deterministic
// unit gets caught -- CI renders twice and refuses on any difference -- so
// that the applier never has to (render.BuildStable's own doc comment: a
// second render there would only re-answer a question the gate already
// asks).
func cmdRenderDigest(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: truss render-digest <dir>")
		return 2
	}
	dir := args[0]

	r := render.Runner{
		Bin: kustomizeBin(getenv),
		// PATH and HOME are copied explicitly, never left nil: Runner.Env
		// turns nil into an empty environment on its own, but kustomize
		// genuinely needs PATH to run, and "explicitly given" is the whole
		// point -- apply_cmd.go's buildBaseEnv does the same copy for the
		// same reason, so the renderer's child never inherits this
		// process's full environment by accident.
		Env:    []string{"PATH=" + getenv("PATH"), "HOME=" + getenv("HOME")},
		Stderr: stderr,
	}

	out, err := r.BuildStable(ctx, dir)
	if err != nil {
		fmt.Fprintf(stderr, "render-digest: %v\n", err)
		return 1
	}
	// No trailing newline: this is filed under the plan-digest prefix and
	// compared byte-for-byte, the same contract plan-digest's own stdout
	// keeps (digest_cmd.go).
	fmt.Fprint(stdout, render.Digest(out))
	return 0
}
