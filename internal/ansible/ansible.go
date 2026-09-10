// Package ansible drives the ansible-playbook binary that configures a
// machine, and reads back what it did.
//
// It is the configuration-side counterpart of internal/render and
// internal/plan, and it is gated differently from both, on purpose. A plan
// is a function of the tree and live infrastructure; a render is a function
// of the tree alone; a play is neither -- it is a function of the tree AND
// the live state of a specific machine, reached over a network CI cannot
// join (docs/design.md, KindAnsible's own doc comment in
// internal/repo/units.go). CI never runs a play and never files anything
// for internal/gates to compare it against: whatever CI could file would be
// a function of the commit alone, and the commit is already pinned by the
// merge-provenance gate, so a play digest would be a check that cannot
// fail. What review means for a play is the diff itself, the same precedent
// docs/credentials.md states for the credentials root -- plus the target
// check in internal/gates, which this package's callers feed from whichever
// provider of host evidence vouches for each host -- internal/tailnet's
// Reconcile for a machine on the tailnet, internal/reach for one at an
// address its own inventory record states.
package ansible

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner drives the ansible-playbook binary.
type Runner struct {
	// Bin is the ansible-playbook executable. Required: there is no
	// default, so a caller that forgot to configure one is refused rather
	// than silently resolving whatever is first on PATH -- the same rule
	// render.Runner.Bin states for the same reason.
	Bin string

	// Env is the EXACT environment the child gets. Nil means an empty
	// environment, never an inherited one: exec.Cmd falls back to
	// os.Environ() when Env is nil, which would hand a play the applier's
	// whole environment -- every cloud credential the process holds,
	// reachable from inside an arbitrary task on someone else's machine.
	// render.Runner.Env and plan.Runner.Env carry the identical rule for
	// the identical reason.
	Env []string

	// Stderr receives everything ansible-playbook writes there. Required,
	// like Bin. Everything the tool writes to stderr goes here and never
	// into a returned error -- plan.wrapExecError's reasoning applies
	// unchanged: an error string here reaches the ledger and a chat
	// message, and a play's transcript can carry host detail (a hostname,
	// a task's output, a file path on someone's machine) that does not
	// belong in either.
	Stderr io.Writer
}

// Result is what one run reported.
type Result struct {
	// ChangedByHost is the per-host changed-task count, parsed from
	// ansible's JSON callback output.
	ChangedByHost map[string]int
	// Raw is the machine-readable output, verbatim, for the caller to
	// record -- the play's counterpart of the bytes render.Runner.Build
	// returns, which a caller hashes or files as it sees fit.
	Raw []byte
}

// ansibleStdoutCallback pins the run to ansible's own JSON callback plugin.
// It is appended to the child's environment after the caller's own Env, so
// a caller cannot weaken it by setting the variable itself -- the same
// override shape render.explicitEnv uses for its proxy settings.
//
// ⚠️ THE CHANGED COUNT IS PARSED FROM THIS, NEVER FROM THE HUMAN-READABLE
// RECAP. AGENTS.md's standing rule is to gate on the field, never on
// rendered text: a regex over ansible's PLAY RECAP would have to track
// every version's exact column layout, and a version that reflows it
// silently turns a real per-host count into a coincidentally-parseable
// string. The JSON callback's `stats` object is the field.
const ansibleStdoutCallback = "ANSIBLE_STDOUT_CALLBACK=json"

// Check runs the play in check mode and reports what it WOULD change. It
// makes no change on any host.
func (r Runner) Check(ctx context.Context, playDir string, hosts []string) (Result, error) {
	return r.run(ctx, playDir, hosts, true)
}

// Apply runs the play for real.
func (r Runner) Apply(ctx context.Context, playDir string, hosts []string) (Result, error) {
	return r.run(ctx, playDir, hosts, false)
}

func (r Runner) run(ctx context.Context, playDir string, hosts []string, check bool) (Result, error) {
	if r.Bin == "" {
		return Result{}, fmt.Errorf("ansible: no ansible-playbook binary configured")
	}
	if playDir == "" {
		return Result{}, fmt.Errorf("ansible: no play directory given")
	}
	if r.Stderr == nil {
		return Result{}, fmt.Errorf("ansible: no stderr writer configured")
	}
	// ⚠️ REFUSED BEFORE EXEC, AND NEVER BUILT FROM ANYTHING BUT hosts. A
	// play run with no --limit at all runs against every host the
	// inventory names -- the difference between configuring one machine
	// and configuring the fleet -- and an empty slice is the one input
	// that could produce that flag by accident (strings.Join of nothing is
	// the empty string, which would build "--limit" with no argument
	// rather than dropping the flag).
	if len(hosts) == 0 {
		return Result{}, fmt.Errorf("ansible: refuses to run with no hosts: a play with no --limit runs against every host in the inventory")
	}

	args := []string{playDir, "--limit", strings.Join(hosts, ",")}
	if check {
		args = append(args, "--check")
	}

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = r.explicitEnv()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = r.Stderr

	if err := cmd.Run(); err != nil {
		// No transcript in the error -- see wrapExecError's reasoning on
		// Stderr's doc comment above. Stderr already has the full run.
		return Result{}, fmt.Errorf("ansible: ansible-playbook %s: %v", playDir, exitOnly(err))
	}

	changed, err := parseChanged(stdout.Bytes())
	if err != nil {
		return Result{}, err
	}
	return Result{ChangedByHost: changed, Raw: stdout.Bytes()}, nil
}

// calloutStats is the shape of ansible's JSON callback output that this
// package reads. The callback emits considerably more (plays, tasks, a
// `custom_stats` block); everything else is left unparsed rather than
// modelled, because nothing here needs it and a struct copying fields
// nobody reads is a second place the real shape can drift out from under
// this one.
type calloutStats struct {
	Stats map[string]struct {
		Changed int `json:"changed"`
	} `json:"stats"`
}

// parseChanged reads the per-host changed count out of ansible's JSON
// callback output.
//
// ⚠️ MALFORMED OUTPUT IS ALWAYS AN ERROR, NEVER AN EMPTY Result. An empty
// map and a parse failure both LOOK like "the run changed nothing", which
// is exactly the ambiguity render.Build's empty-output refusal exists to
// avoid on the delivery side -- a caller that cannot tell "nothing changed"
// from "I could not read what changed" cannot record either fact honestly.
func parseChanged(raw []byte) (map[string]int, error) {
	var parsed calloutStats
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("ansible: could not parse the JSON callback output: %v", err)
	}
	if parsed.Stats == nil {
		return nil, fmt.Errorf("ansible: JSON callback output has no stats object")
	}
	out := make(map[string]int, len(parsed.Stats))
	for host, s := range parsed.Stats {
		out[host] = s.Changed
	}
	return out, nil
}

// explicitEnv returns a non-nil slice always, so exec.Cmd never falls back
// to the parent's environment, and always ends with the callback setting so
// a caller cannot unset it by supplying their own -- later entries win in
// exec, the same override render.Runner.explicitEnv relies on for its proxy
// variables.
func (r Runner) explicitEnv() []string {
	env := []string{}
	if r.Env != nil {
		env = append(env, r.Env...)
	}
	return append(env, ansibleStdoutCallback)
}

// exitOnly reduces an exec error to its status, mirroring
// render.exitOnly/plan.wrapExecError: the transcript is already on Stderr,
// where an operator reads it, and repeating it in the error would put it in
// the ledger and the alert as well.
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
