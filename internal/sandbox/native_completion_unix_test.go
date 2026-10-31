//go:build !windows

package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A successful harness exit and successful process-group cleanup do not prove
// that every storage consumer has exited. The detached child writes only after
// runNativeCommand has returned successfully; pipes supply both barriers.
func TestNativeCommandSuccessCanLeaveStorageConsumer(t *testing.T) {
	switch os.Getenv("SWARM_COMPLETION_FIXTURE_ROLE") {
	case "child":
		ready := os.NewFile(3, "ready")
		fmt.Fprintln(ready, os.Getpid())
		ready.Close()
		var b [1]byte
		if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("SWARM_COMPLETION_FIXTURE_OUTPUT"), []byte("still using storage"), 0600); err != nil {
			os.Exit(3)
		}
		fmt.Fprintln(os.Stdout, "finished")
		os.Exit(0)
	case "parent":
		r, w, err := os.Pipe()
		if err != nil {
			os.Exit(4)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$")
		cmd.Env = append(os.Environ(), "SWARM_COMPLETION_FIXTURE_ROLE=child")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.ExtraFiles = []*os.File{w}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			os.Exit(5)
		}
		w.Close()
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil {
			os.Exit(6)
		}
		fmt.Fprint(os.Stdout, line)
		os.Exit(0)
	}
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer output.Close()
	deadline := time.Now().Add(20 * time.Second)
	if err := reader.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	path := filepath.Join(t.TempDir(), "consumer-output")
	env := append(os.Environ(), "SWARM_COMPLETION_FIXTURE_ROLE=parent", "SWARM_COMPLETION_FIXTURE_OUTPUT="+path)
	if err := runNativeCommand(ctx, os.Args[0], t.TempDir(), env, []string{"-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$"}, input, output, output); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewReader(reader)
	line, err := scan.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("child readiness: %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if _, err = writer.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if line, err = scan.ReadString('\n'); err != nil || line != "finished\n" {
		t.Fatalf("child completion: %q: %v", line, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "still using storage" {
		t.Fatalf("consumer after successful return: %q: %v", data, err)
	}
}
