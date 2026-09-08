// This file is the exec half of package plan: it shells out to `tofu` for
// init, plan, apply and show, and enforces the invariants the bash applier
// paid for. See docs/port-plan.md §4.3b and §2 items 4, 7, 9 and 17.
package plan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Runner drives the `tofu` binary. Every method builds an exact argv, runs
// it in dir, and sends every byte tofu writes to Stderr — never to this
// process's real stdout, because a caller's stdout can be a JSON channel
// (§2 item 17). Env is the exact child environment: it is never merged with
// or defaulted to the calling process's environment (§2 item 9's leak was
// exactly that, on 2026-09-07).
type Runner struct {
	Bin, PluginDir string
	Stderr         io.Writer
	Env            []string // exact environment; never inherits os.Environ implicitly
}

// ErrLockBusy is returned when, and only when, tofu's own output names a
// held state lock. A busy lock is contention between two appliers, not a
// fault, and must never be filed as a failure (§2 item 7).
var ErrLockBusy = errors.New("plan: state lock held elsewhere")

const lockMessage = "Error acquiring the state lock"

// Init runs `tofu init` with the two flags that keep it off the network:
// -plugin-dir, so providers resolve only from the image's baked-in
// directory, and -lockfile=readonly, so init cannot silently rewrite the
// reviewed provider set (§2 item 4).
func (r Runner) Init(ctx context.Context, dir string) error {
	args := []string{
		"init",
		"-backend-config=backend.hcl",
		"-lockfile=readonly",
		"-input=false",
		"-plugin-dir=" + r.PluginDir,
	}
	out, err := r.run(ctx, dir, args)
	return wrapExecError("init", dir, out, err)
}

// Plan runs `tofu plan`, writing the plan to outFile.
func (r Runner) Plan(ctx context.Context, dir, outFile string) error {
	if outFile == "" {
		return errors.New("plan: refuses to plan without an output file")
	}
	args := []string{"plan", "-out=" + outFile, "-input=false", "-no-color"}
	out, err := r.run(ctx, dir, args)
	return wrapExecError("plan", dir, out, err)
}

// PlanDetailed runs the drift check: `-detailed-exitcode -lock=false`, no
// -out, so it can never write a plan file (§2 item 15). Exit 0 means no
// changes; exit 2 means the code and the deployed state disagree, which is
// drift, not an error; any other outcome is a real failure.
func (r Runner) PlanDetailed(ctx context.Context, dir string) (changes bool, err error) {
	args := []string{"plan", "-detailed-exitcode", "-lock=false", "-input=false", "-no-color"}
	out, runErr := r.run(ctx, dir, args)
	if runErr == nil {
		return false, nil
	}
	if lockBusy(out) {
		return false, ErrLockBusy
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 2 {
		return true, nil
	}
	// No transcript and no dir here either -- see wrapExecError.
	return false, runErr
}

// Apply runs `tofu apply` against exactly the plan file it was given —
// never -auto-approve, never a re-plan, always the plan a human already
// reviewed (§2 item 18).
func (r Runner) Apply(ctx context.Context, dir, planFile string) error {
	if planFile == "" {
		return errors.New("apply: refuses to apply without a plan file")
	}
	args := []string{"apply", "-input=false", "-no-color", planFile}
	out, err := r.run(ctx, dir, args)
	return wrapExecError("apply", dir, out, err)
}

// ShowJSON runs `tofu show -json <planFile>` and returns its stdout.
// planFile is required: `show -json` with no file argument prints current
// STATE, which has no resource_changes at all, so a caller that dropped the
// argument would silently digest an empty plan and match anything (§2
// item... see docs/port-plan.md §4.3b).
func (r Runner) ShowJSON(ctx context.Context, dir, planFile string) ([]byte, error) {
	if planFile == "" {
		return nil, errors.New("show -json: refuses to run without a plan file")
	}
	args := []string{"show", "-json", planFile}

	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Env = explicitEnv(r.Env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if r.Stderr != nil && stderr.Len() > 0 {
		_, _ = r.Stderr.Write(stderr.Bytes())
	}
	if runErr != nil {
		combined := stdout.String() + stderr.String()
		if lockBusy(combined) {
			return nil, ErrLockBusy
		}
		return nil, fmt.Errorf("tofu show -json failed in %s: %w\n%s", dir, runErr, stderr.String())
	}
	return stdout.Bytes(), nil
}

// run executes tofu with args in dir, capturing stdout and stderr together
// (the bash's own `2>&1`) and forwarding every byte of that combined output
// to r.Stderr — never to this process's real stdout. It returns the
// combined output (for lock-message matching and for embedding in an error)
// alongside cmd.Run's error, unmodified.
func (r Runner) run(ctx context.Context, dir string, args []string) (combined string, err error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = dir
	cmd.Env = explicitEnv(r.Env)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	if r.Stderr != nil {
		_, _ = r.Stderr.Write(buf.Bytes())
	}
	return buf.String(), runErr
}

// wrapExecError turns a raw exec error and its captured output into either
// ErrLockBusy or a short error naming the step. It returns nil when err is
// nil.
//
// ⚠️ IT DELIBERATELY DOES NOT CARRY TOFU'S OUTPUT, AND IT USED TO. The error
// returned here becomes the pass's failure reason, which is written to a
// ledger object anyone holding the bucket credential can read AND sent to a
// Telegram chat. Handing that a provider's full transcript means whatever
// the provider chose to print -- a request body, a resource attribute, a
// token in an error string -- lands in both. OpenTofu redacts values it
// knows are sensitive; it makes no promise about what a provider writes in
// an error. TrimReason's 800-byte cap does not help: it yields 800 bytes of
// provider output rather than none.
//
// This is not a new judgement. apply.sh:170-180 carries the same reasoning
// under its own "Security review, 2026-09-07", and the port reintroduced
// exactly what that review removed; the parity harness printed it as a diff
// on 2026-09-08, which is what the harness is for.
//
// Nothing is lost: run() has already written the full combined output to
// r.Stderr, which is the pod log. The dir is dropped for the same reason --
// it is the pod's absolute working directory, useful in a log and noise in
// an alert.
func wrapExecError(step, dir, out string, err error) error {
	if err == nil {
		return nil
	}
	if lockBusy(out) {
		return ErrLockBusy
	}
	// The raw exec error and nothing else: "exit status 1". Every caller
	// already names the step and the root ("tofu plan failed for %s: %v"),
	// so wrapping it here would only duplicate that in the alert. What is
	// deliberately kept over the bash's wording is the exit status itself,
	// which distinguishes a tofu that ran and refused from a tofu that could
	// not be executed at all -- the bash reports both identically.
	return err
}

// lockBusy matches OpenTofu's own state-lock message and nothing looser
// (§2 item 7): a busy lock is contention, not a fault, and must be told
// apart from every other failure exactly this way.
func lockBusy(output string) bool {
	return bytes.Contains([]byte(output), []byte(lockMessage))
}

// explicitEnv guarantees the child process never inherits this process's
// environment by accident. exec.Cmd treats a nil Env as "use my own
// environment" (os/exec docs); Runner.Env's zero value is nil, so a Runner
// left at its zero value must still produce an empty, explicit environment
// rather than silently falling back to os.Environ().
func explicitEnv(env []string) []string {
	if env == nil {
		return []string{}
	}
	return env
}
