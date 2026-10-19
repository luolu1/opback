//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// configureHookCmd puts the shell in its own process group and makes context
// cancellation kill the whole group. Without this, a timed-out `sh -c "sleep
// 60"` would leave the sleep running and hold the output pipe open, so
// CombinedOutput would not return until the child exited on its own.
func configureHookCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
}
