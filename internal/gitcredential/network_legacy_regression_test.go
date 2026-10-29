package gitcredential

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file deliberately uses only APIs present before ISSUE-288. Copy it alone
// to the approved base to prove the old helpers fail at runtime, not compilation.
func TestLegacyCompatibleNetworkBound(t *testing.T) {
	for _, op := range []string{"fetch", "push", "ls-remote"} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			release := filepath.Join(root, "release")
			exited := filepath.Join(root, "exited")
			script := filepath.Join(root, "ssh")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			var done = make(chan struct{})
			started := false
			cleanup := func() {
				_ = os.WriteFile(release, nil, 0600)
				if started {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("operation failed to join after fixture release")
					}
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, e := os.Stat(exited); e == nil {
						return
					}
					if time.Now().After(deadline) {
						t.Error("holder exit acknowledgement missing")
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			t.Cleanup(func() {
				if started {
					cleanup()
				}
			})
			body := fmt.Sprintf("#!/bin/sh\ntrap 'touch %s' EXIT\nwhile [ ! -f %s ]; do sleep .05 & wait $!; done\n", quote(exited), quote(release))
			if e := os.WriteFile(script, []byte(body), 0700); e != nil {
				t.Fatal(e)
			}
			git := func(args ...string) {
				t.Helper()
				c := exec.Command("git", args...)
				c.Dir = root
				if b, e := c.CombinedOutput(); e != nil {
					t.Fatalf("git setup: %v %s", e, b)
				}
			}
			git("init")
			git("config", "core.sshCommand", quote(script))
			git("config", "ssh.variant", "simple")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "candidate")
			args := []string{op, "ssh://fake.invalid/fixture"}
			if op == "push" {
				args = append(args, "HEAD:refs/heads/candidate")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			var result error
			started = true
			go func() {
				defer close(done)
				if os.Getenv("SWARM_TEST_LEGACY_GIT") == "1" {
					cmd, _, err := Command(ctx, root, args...)
					if err == nil {
						_, err = cmd.CombinedOutput()
					}
					result = err
				} else {
					_, result = Output(ctx, root, args...)
				}
			}()
			watchdog := 20 * time.Second
			if os.Getenv("SWARM_TEST_LEGACY_GIT") == "1" {
				watchdog = 8 * time.Second
			}
			select {
			case <-done:
				if result == nil {
					t.Error("hanging transport unexpectedly succeeded")
				}
				if elapsed := time.Since(start); elapsed > 8*time.Second {
					t.Errorf("return exceeded execution + reap + I/O bound: %s", elapsed)
				}
				t.Logf("%s returned after %s: %v", op, time.Since(start), result)
			case <-time.After(watchdog):
				t.Errorf("unbounded legacy helper: %s still blocked after %s despite caller cancellation", op, watchdog)
			}
			cleanup()
		})
	}
}
