//go:build !windows

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/process"
	"github.com/supernovae/swarmos/internal/storageuse"
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
	if err := runLegacyNativeCommand(ctx, os.Args[0], t.TempDir(), env, []string{"-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$"}, input, output, output, process.StopVerified); err != nil {
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

func init() { runCompletionFixture = nativeCompletionFixture }
func nativeCompletionFixture() int {
	role := os.Getenv("SWARM_COMPLETION_FIXTURE_ROLE")
	cache, err := userCacheDir()
	if err != nil {
		return 8
	}
	ledger, err := storageuse.UserCacheDir()
	if err != nil {
		return 9
	}
	if cache != os.Getenv("HOME") || cache != os.Getenv("XDG_CACHE_HOME") || cache == ledger || liveExecutorGoCache != "" {
		return 10
	}
	if audit := os.Getenv("SWARM_COMPLETION_AUDIT"); audit != "" {
		data, _ := json.Marshal([]string{cache, ledger})
		if err := os.WriteFile(filepath.Join(audit, role+".json"), data, 0600); err != nil {
			return 11
		}
	}
	if os.Getenv("SWARM_COMPLETION_FAIL") == role {
		return 12
	}

	switch os.Getenv("SWARM_COMPLETION_FIXTURE_ROLE") {
	case "child":
		ready := os.NewFile(3, "ready")
		fmt.Fprintln(ready, os.Getpid())
		ready.Close()
		var b [1]byte
		if _, err := io.ReadFull(os.Stdin, b[:]); err != nil {
			return (2)
		}
		if err := os.WriteFile(os.Getenv("SWARM_COMPLETION_FIXTURE_OUTPUT"), []byte("still using storage"), 0600); err != nil {
			return (3)
		}
		fmt.Fprintln(os.Stdout, "finished")
		return (0)
	case "parent":
		r, w, err := os.Pipe()
		if err != nil {
			return (4)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$")
		cmd.Env = append(os.Environ(), "SWARM_COMPLETION_FIXTURE_ROLE=child")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.ExtraFiles = []*os.File{w}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return (5)
		}
		w.Close()
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil {
			return (6)
		}
		fmt.Fprint(os.Stdout, line)
		return (0)
	}
	return 7
}

func TestNativeCompletionFixtureDispatch(t *testing.T) {
	for _, role := range []string{"", "parent", "child", "unknown"} {
		if got, want := completionFixtureRole(role), role == "parent" || role == "child"; got != want {
			t.Fatalf("role %q: %v", role, got)
		}
	}
}

func TestNativeCompletionFixtureSkipsLiveSetup(t *testing.T) {
	// The fixture's cwd has no manifests: entering live prefetch would fail before
	// its pipe protocol. This exercises both re-execution levels with live enabled.
	t.Setenv("SWARM_TEST_EXECUTOR", "1")
	audit := t.TempDir()
	t.Setenv("SWARM_COMPLETION_AUDIT", audit)
	TestNativeCommandSuccessCanLeaveStorageConsumer(t)
	assertCompletionRootsRemoved(t, audit, "parent", "child")
	for _, role := range []string{"parent", "child"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCommandSuccessCanLeaveStorageConsumer$")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "SWARM_COMPLETION_FIXTURE_ROLE="+role, "SWARM_COMPLETION_FAIL="+role)
		if err := cmd.Run(); err == nil {
			t.Fatal("fixture failure unexpectedly succeeded")
		}
		assertCompletionRootsRemoved(t, audit, role)
	}
}

func assertCompletionRootsRemoved(t *testing.T, audit string, roles ...string) {
	t.Helper()
	for _, role := range roles {
		data, err := os.ReadFile(filepath.Join(audit, role+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var roots []string
		if err = json.Unmarshal(data, &roots); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for _, root := range roots {
			for {
				_, err = os.Stat(root)
				if os.IsNotExist(err) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("root not removed: %s: %v", root, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}
