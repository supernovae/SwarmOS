//go:build !windows

package storageuse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The pipe is the crash barrier: admission is durable before the parent kills
// the coordinator. The stable lock is released by the OS, but the use remains.
func TestCoordinatorCrash(t *testing.T) {
	if root := os.Getenv("SWARM_STORAGEUSE_TEST_ROOT"); root != "" {
		m, err := Open(filepath.Join(root, "control"))
		if err != nil {
			panic(err)
		}
		r := Resource{Repository: "repo", Class: "scratch", Identity: "crash", Location: filepath.Join(root, "data")}
		if _, err = m.AcquireUse(context.Background(), r); err != nil {
			panic(err)
		}
		if os.Getenv("SWARM_STORAGEUSE_TEST_CREATE") == "yes" {
			if err = os.Mkdir(r.Location, 0700); err != nil {
				panic(err)
			}
		}
		fmt.Println("admitted")
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		os.Exit(0)
	}
	for _, created := range []string{"no", "yes"} {
		t.Run(created, func(t *testing.T) {
			root := canonicalTemp(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinatorCrash$")
			cmd.Env = append(os.Environ(), "SWARM_STORAGEUSE_TEST_ROOT="+root, "SWARM_STORAGEUSE_TEST_CREATE="+created)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || line != "admitted\n" {
				t.Fatalf("barrier: %q %v", line, err)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			m, err := Open(filepath.Join(root, "control"))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			r := Resource{Repository: "repo", Class: "scratch", Identity: "crash", Location: filepath.Join(root, "data")}
			if g, err := m.AcquireMaintenance(context.Background(), r); !errors.Is(err, ErrUnsafe) {
				if g != nil {
					g.Close()
				}
				t.Fatalf("crash lost use: %v", err)
			}
		})
	}
}

func TestCoordinatorSurvivingChild(t *testing.T) {
	root := os.Getenv("SWARM_STORAGEUSE_DESCENDANT_ROOT")
	r := Resource{Repository: "repo", Class: "scratch", Identity: "descendant", Location: filepath.Join(root, "data")}
	switch os.Getenv("SWARM_STORAGEUSE_DESCENDANT_ROLE") {
	case "child":
		f, err := os.OpenFile(filepath.Join(r.Location, "consumer"), os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		fmt.Printf("child %d\n", os.Getpid())
		var b [1]byte
		if _, err = os.Stdin.Read(b[:]); err != nil {
			panic(err)
		}
		if _, err = f.WriteString("consumer survived coordinator\n"); err != nil {
			panic(err)
		}
		if err = f.Close(); err != nil {
			panic(err)
		}
		fmt.Println("consumer-finished")
		os.Exit(0)
	case "coordinator":
		m, err := Open(filepath.Join(root, "control"))
		if err != nil {
			panic(err)
		}
		if _, err = m.AcquireUse(context.Background(), r); err != nil {
			panic(err)
		}
		if err = os.Mkdir(r.Location, 0700); err != nil {
			panic(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinatorSurvivingChild$")
		cmd.Env = append(os.Environ(), "SWARM_STORAGEUSE_DESCENDANT_ROLE=child")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err = cmd.Start(); err != nil {
			panic(err)
		}
		fmt.Println("coordinator-ready")
		select {}
	}
	root = canonicalTemp(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinatorSurvivingChild$")
	cmd.Env = append(os.Environ(), "SWARM_STORAGEUSE_DESCENDANT_ROOT="+root, "SWARM_STORAGEUSE_DESCENDANT_ROLE=coordinator")
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scan := bufio.NewScanner(out)
	pid := 0
	ready := false
	for (pid == 0 || !ready) && scan.Scan() {
		if scan.Text() == "coordinator-ready" {
			ready = true
		}
		if strings.HasPrefix(scan.Text(), "child ") {
			pid, err = strconv.Atoi(strings.TrimPrefix(scan.Text(), "child "))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if pid == 0 || !ready {
		t.Fatalf("missing child barrier: %v", scan.Err())
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, 0); err != nil {
		t.Fatalf("detached child did not survive: %v", err)
	}
	m, err := Open(filepath.Join(root, "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	r.Location = filepath.Join(root, "data")
	blocked := func() {
		t.Helper()
		if g, err := m.AcquireMaintenance(context.Background(), r); !errors.Is(err, ErrUnsafe) {
			if g != nil {
				g.Close()
			}
			t.Fatalf("lost descendant protection: %v", err)
		}
	}
	blocked()
	if _, err = in.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	// Wait for positive child completion through the inherited pipe. The
	// coordinator was killed and cannot durably record that completion.
	finished := false
	for scan.Scan() {
		if scan.Text() == "consumer-finished" {
			finished = true
			break
		}
	}
	if !finished {
		t.Fatalf("child completion barrier failed: %v", scan.Err())
	}
	data, err := os.ReadFile(filepath.Join(r.Location, "consumer"))
	if err != nil || string(data) != "consumer survived coordinator\n" {
		t.Fatalf("surviving consumer: %q %v", data, err)
	}
	blocked()
}

// The coordinator dies at a durable reservation/acknowledgement barrier. Empty
// observations are injected: kernel containment has a separate supported-host test.
func TestNativeCoordinatorCrash(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native recovery requires Linux identities")
	}
	if root := os.Getenv("SWARM_NATIVE_CRASH_ROOT"); root != "" {
		m, err := Open(filepath.Join(root, "control"))
		if err != nil {
			panic(err)
		}
		r := Resource{Repository: "repo", Class: "agent-build", Identity: "native-crash", Location: filepath.Join(root, "data")}
		s := &Scope{manager: m, repository: r.Repository}
		if err = s.AcquirePrepared(context.Background(), r); err != nil {
			panic(err)
		}
		b, err := s.ReserveNative(context.Background())
		if err != nil {
			panic(err)
		}
		if os.Getenv("SWARM_NATIVE_CRASH_MODE") != "reserved" {
			n := NativeIdentity{Boot: "11111111-1111-1111-1111-111111111111", Unit: "swarm-native-0123456789abcdef0123456789abcdef.scope", Group: "/user.slice/swarm-native-0123456789abcdef0123456789abcdef.scope", MountDevice: 1, MountInode: 2, Inode: 3}
			if err = b.Seal(context.Background(), n); err != nil {
				panic(err)
			}
		}
		fmt.Fprintln(os.Stdout, "durable")
		var bfr [1]byte
		_, _ = os.Stdin.Read(bfr[:])
		os.Exit(0)
	}
	for _, mode := range []string{"reserved", "sealed"} {
		t.Run(mode, func(t *testing.T) {
			root := canonicalTemp(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeCoordinatorCrash$")
			cmd.Env = append(os.Environ(), "SWARM_NATIVE_CRASH_ROOT="+root, "SWARM_NATIVE_CRASH_MODE="+mode)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "durable\n" {
				t.Fatalf("barrier %q %v", line, err)
			}
			m, err := Open(filepath.Join(root, "control"))
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			r := Resource{Repository: "repo", Class: "agent-build", Identity: "native-crash", Location: filepath.Join(root, "data")}
			// No recovery while the authoritative coordinator identity is still alive.
			if changed, err := m.recoverNative(context.Background(), r, time.Now(), processGone, func(NativeIdentity) bool { return true }); err != nil || changed {
				t.Fatalf("live coordinator recovery %v %v", changed, err)
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if changed, err := m.recoverNative(context.Background(), r, time.Now(), processGone, func(NativeIdentity) bool { return false }); err != nil || changed {
				t.Fatalf("live scope recovery %v %v", changed, err)
			}
			now := time.Now().Add(time.Hour)
			changed, err := m.recoverNative(context.Background(), r, now, processGone, func(NativeIdentity) bool { return true })
			if err != nil || changed != (mode == "sealed") {
				t.Fatalf("empty scope recovery %v %v", changed, err)
			}
			if mode == "sealed" {
				guard, err := m.AcquireMaintenance(context.Background(), r)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
				if !guard.Observation().LastUse.Equal(now) {
					t.Fatal("recovery did not restart retention")
				}
			}
		})
	}
}
