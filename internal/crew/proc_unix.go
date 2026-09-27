//go:build unix

package crew

import (
	"os/exec"
	"syscall"
	"time"
)

// gracePeriod is how long a cancelled job has to exit on SIGTERM before it is
// killed outright: long enough for a build to flush its output, short enough
// that the operator is not left waiting.
const gracePeriod = 5 * time.Second

// setProcessGroup puts the child in its own process group so cancelling a job
// kills the whole tree. An agent spawns compilers, containers and shells;
// killing only the parent would leave those running.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup signals the job's process group and returns the timer for
// the follow-up SIGKILL, so the caller can stop it once the process is reaped.
// Leaving it armed risks signalling a process group id the kernel has reused.
func killProcessGroup(cmd *exec.Cmd) *time.Timer {
	if cmd.Process == nil {
		return nil
	}
	pgid := -cmd.Process.Pid
	// Ask politely, then insist.
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	return time.AfterFunc(gracePeriod, func() { _ = syscall.Kill(pgid, syscall.SIGKILL) })
}
