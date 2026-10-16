//go:build linux

package agent

import (
	"os/exec"
	"syscall"
)

// detachCleanup runs name+args in a brand-new session so the child keeps
// running after this process exits. When the agent runs under systemd with
// KillMode=process, stopping the unit does not signal the detached child.
func detachCleanup(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
