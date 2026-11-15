//go:build !windows

package process

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func Configure(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func Stop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// StopVerified reports failures to terminate the process group and waits
// briefly for SIGKILLed descendants to finish kernel exit teardown.
func StopVerified(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return nil
		}
		if dead, known := procGroupExited(pgid); known && dead {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// procGroupExited checks /proc (when mounted, as on Linux) to see whether every
// remaining member of pgid has reached zombie/dead state after SIGKILL.
func procGroupExited(pgid int) (dead, known bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	sawPid := false
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		sawPid = true
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		end := bytes.LastIndexByte(raw, ')')
		if end < 0 || end+2 >= len(raw) {
			continue
		}
		fields := bytes.Fields(raw[end+2:])
		if len(fields) < 3 {
			continue
		}
		pgrp, err := strconv.Atoi(string(fields[2]))
		if err != nil || pgrp != pgid {
			continue
		}
		state := fields[0][0]
		if state != 'Z' && state != 'X' && state != 'x' {
			return false, true
		}
	}
	return sawPid, sawPid
}
