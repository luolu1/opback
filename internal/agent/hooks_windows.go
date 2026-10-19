//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// configureHookCmd starts the command in a new process group and kills the
// whole tree on cancellation. cmd.exe leaves its children (dump tools, sleep
// equivalents) running otherwise.
func configureHookCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		_ = kill.Run()
		return nil
	}
}
