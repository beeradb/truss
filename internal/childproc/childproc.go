// Package childproc builds every external command this binary runs. It
// exists because three properties of a child process have to hold at every
// call site, and a property enforced by five copies of four lines is a
// property that will be true at four of them.
package childproc

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// killDelay is how long a child gets to act on its own interrupt before this
// package escalates to SIGKILL. Long enough for tofu's own "Gracefully
// shutting down..." path (measured below) to finish releasing the state
// lock; short enough that a child which ignores the interrupt cannot pin
// this process open through a graceful stop indefinitely.
const killDelay = 10 * time.Second

// Command returns an exec.Cmd for ctx that:
//
//   - runs in its OWN process group, so a signal aimed at truss's group -- a
//     Ctrl-C at a terminal, a `kill -TERM -<pgid>` from a supervisor -- does
//     not reach a running `tofu apply` before truss has decided what to do
//     about it. Kubernetes itself signals PID 1 only, never a process
//     group, so this buys nothing against the kubelet; it buys everything
//     against every other way a signal reaches this process.
//
//   - on ctx cancellation, sends SIGINT to the child's process group rather
//     than letting exec.CommandContext's default apply, which is to SIGKILL
//     the child immediately. Measured against the real binary this project
//     pins (OpenTofu 1.12.6, 2026-09-12, a 20s time_sleep resource held
//     mid-create): SIGINT prints "Interrupt received... Gracefully shutting
//     down..." and exits with the local state lock released and no
//     orphaned provider process. SIGTERM to the same process, same
//     conditions, does neither -- tofu dies immediately (exit via signal),
//     leaves the lock file in place, and orphans the provider subprocess.
//     Sending SIGTERM here would recreate the exact leaked-lock failure
//     this package exists to prevent.
//
//   - is killed killDelay later if it ignores that, so a child which traps
//     the signal and does nothing cannot pin this process open forever.
//
// The negative pid in the Kill call is the process group: Setpgid below
// makes the child its own group leader, so its pgid equals its pid, and
// signalling the group rather than the process is what also reaches tofu's
// provider plugins -- confirmed in the same measurement, where a SIGTERM
// left the provider process running after tofu itself had died.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	}
	cmd.WaitDelay = killDelay
	return cmd
}
