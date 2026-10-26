//go:build windows

package process

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func Stop(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	_ = cmd.Process.Kill()
}

// StopVerified is unavailable for snapshot import on Windows.
func StopVerified(cmd *exec.Cmd) error {
	Stop(cmd)
	return fmt.Errorf("verified snapshot process termination requires a qualified Linux worker")
}
