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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// playEntrypoint is the file INSIDE a play directory that ansible-playbook
// is actually handed. Callers pass the directory, because everywhere else in
// this system a play IS a directory.
//
// ⚠️ AND THE OBVIOUS WORKAROUND -- POINTING A HOST RECORD AT THE FILE --
// FAILS SILENTLY, WHICH IS WHY THE JOIN LIVES HERE AND NOT IN THE
// INVENTORY. git.TreeAnsibleUnits lists plays with `ls-tree -d`, so only a
// directory can ever BE a unit; repo.KindOf classifies ansible/plays/<name>/
// and rejects the directory above it; and playHosts matches a host's
// `config` against the unit path by EXACT equality. A record saying
// ansible/plays/<name>/site.yml therefore matches no unit at all: the play
// runs against zero hosts, reports applied=0, and reads as a clean pass.
// That silent no-op is strictly worse than the error this constant fixes,
// so the directory stays the unit and the entrypoint is named right here.
//
// Measured 2026-09-10: a real pass failed with ansible's own "the playbook:
// /work/repo/ansible/plays/dev-workstation does not appear to be a file",
// which names the path we built and not the convention it broke.
const playEntrypoint = "site.yml"

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

// ansibleGroupChars pins the group names in the generated inventory to the
// names truss wrote, and is appended after the caller's Env for the same
// reason ansibleStdoutCallback is.
//
// ⚠️ AN AMBIENT ENVIRONMENT VARIABLE CAN OTHERWISE RENAME OUR GROUPS, AND
// SILENTLY. Groups are derived from a host's role, and this deployment's
// roles carry hyphens -- "dev-workstation" -- which ansible considers an
// invalid character in a group name. Measured against ansible 2.16.3:
// unset, the group is "dev-workstation" and a warning is printed; with
// ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS=always it becomes
// "dev_workstation" with no warning at all, so group_vars/dev-workstation/
// would quietly stop resolving and a play would run with variables it had
// always had until somebody exported a variable on the applier.
//
// "never" is also the current default, which is exactly why it is written
// down: a default is a thing that can change, and a behaviour this file
// depends on should be stated rather than inherited.
const ansibleGroupChars = "ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS=never"

// Check runs the play in check mode and reports what it WOULD change. It
// makes no change on any host.
func (r Runner) Check(ctx context.Context, playDir string, targets []Target) (Result, error) {
	return r.run(ctx, playDir, targets, true)
}

// Apply runs the play for real.
func (r Runner) Apply(ctx context.Context, playDir string, targets []Target) (Result, error) {
	return r.run(ctx, playDir, targets, false)
}

func (r Runner) run(ctx context.Context, playDir string, targets []Target, check bool) (Result, error) {
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
	if len(targets) == 0 {
		return Result{}, fmt.Errorf("ansible: refuses to run with no hosts: a play with no --limit runs against every host in the inventory")
	}

	// ⚠️ STATTED BEFORE EXEC so the refusal names the convention rather
	// than the path. ansible's own message for this is "does not appear to
	// be a file", which sends a reader looking at the path we constructed
	// instead of at the missing site.yml -- it cost a debugging pass in
	// the wrong layer on 2026-09-10. IsRegular, not merely "exists": a
	// directory named site.yml would satisfy a bare Stat and then fail
	// inside ansible with that same unhelpful sentence.
	play := filepath.Join(playDir, playEntrypoint)
	if info, err := os.Stat(play); err != nil || !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("ansible: %s has no %s: a play is a directory and %s is its entrypoint", playDir, playEntrypoint, playEntrypoint)
	}

	// ⚠️ THE INVENTORY IS GENERATED PER RUN, AND WITHOUT ONE THE --limit
	// ABOVE NARROWS AN EMPTY SET. Before this, no -i was passed at all, so
	// ansible parsed no inventory, found only the implicit localhost, and
	// answered a play with "Could not match supplied host pattern,
	// ignoring: dev-agent" -- a WARNING, not an error, on the way to
	// reporting a run that configured nothing. Every host truss knows about
	// lives in its own inventory/ records; this is the bridge between that
	// and ansible's idea of an inventory, and it is derived rather than
	// committed so the two cannot disagree.
	//
	// ⚠️ WRITTEN 0600 AND REMOVED AFTERWARDS, IN A DIRECTORY ONLY THIS RUN
	// OWNS. It names every machine in the play and the account each is
	// logged into as, which is a map of the fleet even though no line of it
	// is a credential.
	invDir, err := os.MkdirTemp("", "truss-ansible-inventory")
	if err != nil {
		return Result{}, fmt.Errorf("ansible: creating an inventory directory: %v", err)
	}
	defer os.RemoveAll(invDir)
	inv, err := renderInventory(targets)
	if err != nil {
		return Result{}, err
	}
	invPath := filepath.Join(invDir, "hosts.yml")
	if err := os.WriteFile(invPath, inv, 0o600); err != nil {
		return Result{}, fmt.Errorf("ansible: writing the inventory: %v", err)
	}

	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	sort.Strings(names)

	args := []string{play, "-i", invPath, "--limit", strings.Join(names, ",")}
	if check {
		args = append(args, "--check")
	}

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Env = r.explicitEnv()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = r.Stderr

	if err := cmd.Run(); err != nil {
		// ⚠️ THE RUN'S DETAIL IS ON STDOUT, NOT STDERR, AND THIS PACKAGE IS
		// WHY. ansibleStdoutCallback pins ANSIBLE_STDOUT_CALLBACK=json, so
		// everything ansible has to say about what it did -- which task, on
		// which host, and the message -- goes to STDOUT as JSON, where it is
		// captured into a buffer for parseChanged. Stderr gets warnings.
		//
		// This block used to say "Stderr already has the full run" and
		// return. That was false, and false BECAUSE of the callback pinned
		// forty lines above: on failure the function returns before parsing,
		// and the buffer is discarded. Measured 2026-09-11: a pass failed
		// with two DEPRECATION WARNINGs, "exit status 4", and NO ERROR LINE
		// ANYWHERE. It took three passes to learn that ansible had been
		// explaining itself the whole time, into a buffer nobody read.
		//
		// ⚠️ IT GOES TO Stderr, NOT INTO THE ERROR, and that distinction is
		// the original comment's point and still holds: an error string
		// reaches the ledger and a Telegram message, and a play's transcript
		// can carry a hostname, a task's output, or a path on somebody's
		// machine. The operator reading logs should see it; the alert should
		// not carry it.
		//
		// Raw, not parsed. A failed run is the worst moment to depend on the
		// output being well-formed -- ansible may have died before writing
		// valid JSON at all, which is exactly the case worth seeing.
		if stdout.Len() > 0 {
			fmt.Fprintf(r.Stderr, "\nansible: %s failed; its JSON callback output follows, because the run's detail is on stdout and would otherwise be discarded:\n%s\n", playDir, stdout.Bytes())
		}
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
	return append(env, ansibleStdoutCallback, ansibleGroupChars)
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
