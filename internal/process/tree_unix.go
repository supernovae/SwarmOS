//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

func Configure(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func Stop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
