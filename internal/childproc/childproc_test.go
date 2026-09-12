package childproc

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// ---- a child that traps SIGINT, run as this same test binary ----
//
// The pattern is the one os/exec's own tests use, and internal/plan/runner_test.go
// already follows it: the compiled test binary re-executes itself with a
// sentinel in its environment, and TestMain diverts to the helper instead of
// running go test's usual machinery.

const helperEnvVar = "GO_WANT_HELPER_PROCESS"

// exitInterrupted is the helper's exit code when it caught the interrupt --
// chosen to be unmistakable for anything exec.Cmd itself might report (a
// plain exit-status collision would leave "was it killed or did it exit 1"
// ambiguous).
const exitInterrupted = 77

var testBinary string

func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		runHelper()
		return // unreachable: runHelper always calls os.Exit
	}
	bin, err := os.Executable()
	if err != nil {
		panic(err)
	}
	testBinary = bin
	os.Exit(m.Run())
}

// runHelper blocks until it catches SIGINT, then exits distinctively. It
// never touches SIGTERM, so if Command ever regresses to sending SIGTERM
// (exec.CommandContext's undocumented-here default is SIGKILL, which this
// helper also could not catch), the helper hangs until the test's own
// timeout kills it -- a hang reads as a failure, which is the point.
//
// It prints "ready" before blocking so the parent can wait for the handler
// to actually be installed rather than racing signal.Notify: cancelling the
// context the instant Start returns would otherwise sometimes land the
// SIGINT before this goroutine's Notify call, which the OS then handles
// with its own default disposition (terminate) instead of ours.
func runHelper() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	os.Stdout.WriteString("ready\n")
	<-sig
	os.Exit(exitInterrupted)
}

// startHelper starts the helper and blocks until it has confirmed its
// signal handler is installed, so the caller may cancel ctx immediately
// afterward with no race against the child's own startup.
func startHelper(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	cmd := Command(ctx, testBinary, "-test.run=^$")
	cmd.Env = []string{helperEnvVar + "=1"}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("reading helper readiness: line=%q err=%v", line, err)
	}
	return cmd
}

// TestEveryChildRunsInItsOwnProcessGroup is the Setpgid half of Command's
// contract: signalling this test process's own group must not reach the
// child, which it can only do if the child leads a group of its own.
func TestEveryChildRunsInItsOwnProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := startHelper(t, ctx)

	childPgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid(child): %v", err)
	}
	if childPgid != cmd.Process.Pid {
		t.Fatalf("child pgid = %d, want %d (its own pid): Setpgid did not take effect", childPgid, cmd.Process.Pid)
	}

	ownPgid, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("Getpgid(self): %v", err)
	}
	if childPgid == ownPgid {
		t.Fatalf("child shares this test's process group (%d): a signal to the test's group would reach it directly", ownPgid)
	}

	cancel()
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitInterrupted {
			t.Fatalf("Wait: %v, want exit %d", err, exitInterrupted)
		}
	}
}

// TestACancelledChildIsInterruptedNotKilled is Command's central claim,
// measured against the real tofu binary in the design work and reproduced
// here without it: cancelling ctx must reach the child as SIGINT, which it
// can catch and act on, never as the SIGKILL exec.CommandContext defaults
// to and this helper could not survive to report.
func TestACancelledChildIsInterruptedNotKilled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := startHelper(t, ctx)

	// Cancel almost immediately -- the helper is already blocked on
	// signal.Notify by the time Start returns control here in practice, and
	// if it is not yet, SIGINT queues until it is.
	cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Wait: %v, want an *exec.ExitError carrying exit %d", err, exitInterrupted)
		}
		if exitErr.ExitCode() != exitInterrupted {
			t.Fatalf("exit code = %d, want %d (the helper's own SIGINT handler) -- a different code or a kill signal means Cancel sent something the helper could not catch", exitErr.ExitCode(), exitInterrupted)
		}
	case <-time.After(killDelay):
		t.Fatalf("child did not exit within WaitDelay (%s): Cancel likely sent a signal the helper never caught, and killDelay's SIGKILL should have ended it well before this timeout fired", killDelay)
	}
}
