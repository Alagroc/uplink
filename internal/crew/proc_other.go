//go:build !unix

package crew

import (
	"os/exec"
	"time"
)

// gracePeriod mirrors the unix build; there is no process group to signal here,
// so the kill is immediate and nothing is deferred.
const gracePeriod = 5 * time.Second

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) *time.Timer {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}
