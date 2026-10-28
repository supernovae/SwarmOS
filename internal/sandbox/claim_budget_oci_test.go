package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/supernovae/swarmos/internal/claimpause"
)

type pauseWorkerWriter struct {
	once   sync.Once
	cancel context.CancelCauseFunc
}

func (w *pauseWorkerWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { w.cancel(claimpause.ErrBudget) })
	return len(p), nil
}

func TestOCIClaimBudgetStopsExportsAndImportsInOrder(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake OCI runtime uses a Unix shell")
	}
	for _, failure := range []string{"", "freeze", "export", "termination", "import"} {
		t.Run("failure="+failure, func(t *testing.T) {
			t.Parallel()
			root, dir, result := t.TempDir(), t.TempDir(), t.TempDir()
			writeWorkerFixture(t, dir, "source", "before")
			writeWorkerFixture(t, result, "source", "saved")
			archive := filepath.Join(root, "result.tar")
			if _, err := packWorker(t.Context(), result, archive, nil, false); err != nil {
				t.Fatal(err)
			}
			image := []any{map[string]any{"Os": "linux", "Config": map[string]any{"Volumes": nil}}}
			host := map[string]any{"Privileged": false, "NetworkMode": "none", "PidMode": "private", "IpcMode": "private", "CgroupnsMode": "private", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}, "Memory": int64(2 << 30), "MemorySwap": int64(2 << 30), "NanoCpus": int64(2000000000), "PidsLimit": 128, "Tmpfs": map[string]string{
				"/workspace":   "rw,nosuid,nodev,size=512m,mode=0755,uid=65534,gid=65534",
				"/output":      "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534",
				"/home/worker": "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534",
				"/tmp":         "rw,nosuid,nodev,size=64m,mode=1777,uid=0,gid=0",
			}}
			container := []any{map[string]any{"HostConfig": host, "Mounts": []any{}}}
			for name, value := range map[string]any{"image.json": image, "container.json": container} {
				raw, _ := json.Marshal(value)
				if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			script := `#!/bin/sh
root=` + quote(root) + `
printf '%s\n' "$*" >> "$root/calls"
case "$1" in
 image) cat "$root/image.json";;
 inspect) cat "$root/container.json";;
 create) printf '%s\n' '` + strings.Repeat("a", 64) + `';;
 exec) case "$*" in *worker-payload*) printf worker-started; while :; do :; done;; *) :;; esac;;
 cp) if test "$2" = --archive; then cat >/dev/null; else `
			if failure == "export" {
				script += "exit 19; "
			}
			script += `cat "$root/result.tar"; fi;;
 pause) `
			if failure == "freeze" {
				script += "exit 18"
			} else {
				script += ":"
			}
			script += `;;
 rm) `
			if failure == "termination" {
				script += "exit 17"
			} else {
				script += ":"
			}
			script += ";;\n *) :;;\nesac\n"
			binary := filepath.Join(root, "docker")
			if err := writeFakeExecutable(binary, script); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			ctx = claimpause.WithParent(ctx, t.Context())
			imported := false
			opts := WorkerOptions{Runtime: binary, Image: "local/image@sha256:" + strings.Repeat("b", 64), Role: "developer", CheckImport: func(context.Context, []string, []string) error {
				imported = true
				calls, _ := os.ReadFile(filepath.Join(root, "calls"))
				lines := string(calls)
				pause, export, termination := strings.Index(lines, "pause "), strings.Index(lines, "cp "+strings.Repeat("a", 64)+":/workspace/. -"), strings.Index(lines, "rm --force")
				if pause < 0 || export < pause || termination < export {
					t.Fatalf("unsafe ordering: %s", lines)
				}
				if failure == "import" {
					return errors.New("import rejected")
				}
				return nil
			}}
			err := RunWorker(ctx, dir, "worker-payload", nil, opts, nil, &pauseWorkerWriter{cancel: cancel}, io.Discard)
			data, _ := os.ReadFile(filepath.Join(dir, "source"))
			if failure == "" {
				if !errors.Is(err, claimpause.ErrBudget) || !imported || string(data) != "saved" {
					t.Fatalf("lost paused output: %s %v", data, err)
				}
			} else if err == nil || errors.Is(err, claimpause.ErrBudget) || string(data) != "before" {
				t.Fatalf("failed cleanup imported: %s %v", data, err)
			}
		})
	}
}

func TestOCIWritableImportPreflight(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("fake OCI runtime uses a Unix shell")
	}
	for _, role := range []string{"developer", "tester"} {
		for _, failure := range []string{"", "import"} {
			t.Run(role+"/failure="+failure, func(t *testing.T) {
				t.Parallel()
				root, dir, result := t.TempDir(), t.TempDir(), t.TempDir()
				writeWorkerFixture(t, dir, "source", "before")
				writeWorkerFixture(t, dir, "deleted", "before")
				writeWorkerFixture(t, result, "source", "saved")
				archive := filepath.Join(root, "result.tar")
				if _, err := packWorker(t.Context(), result, archive, nil, false); err != nil {
					t.Fatal(err)
				}
				image := []any{map[string]any{"Os": "linux", "Config": map[string]any{"Volumes": nil}}}
				host := map[string]any{"Privileged": false, "NetworkMode": "none", "PidMode": "private", "IpcMode": "private", "CgroupnsMode": "private", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}, "Memory": int64(2 << 30), "MemorySwap": int64(2 << 30), "NanoCpus": int64(2000000000), "PidsLimit": 128, "Tmpfs": map[string]string{
					"/workspace":   "rw,nosuid,nodev,size=512m,mode=0755,uid=65534,gid=65534",
					"/output":      "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534",
					"/home/worker": "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534",
					"/tmp":         "rw,nosuid,nodev,size=64m,mode=1777,uid=0,gid=0",
				}}
				container := []any{map[string]any{"HostConfig": host, "Mounts": []any{}}}
				for name, value := range map[string]any{"image.json": image, "container.json": container} {
					raw, _ := json.Marshal(value)
					if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
				script := `#!/bin/sh
root=` + quote(root) + `
printf '%s\n' "$*" >> "$root/calls"
case "$1" in
 image) cat "$root/image.json";;
 inspect) cat "$root/container.json";;
 create) printf '%s\n' '` + strings.Repeat("a", 64) + `';;
 exec) :;;
 cp) if test "$2" = --archive; then cat >/dev/null; else `
				if failure == "export" {
					script += "exit 19; "
				}
				script += `cat "$root/result.tar"; fi;;
 pause) `
				if failure == "freeze" {
					script += "exit 18"
				} else {
					script += ":"
				}
				script += `;;
 rm) `
				if failure == "termination" {
					script += "exit 17"
				} else {
					script += ":"
				}
				script += ";;\n *) :;;\nesac\n"
				binary := filepath.Join(root, "docker")
				if err := writeFakeExecutable(binary, script); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(context.Canceled)
				ctx = claimpause.WithParent(ctx, t.Context())
				imported := false
				opts := WorkerOptions{Runtime: binary, Image: "local/image@sha256:" + strings.Repeat("b", 64), Role: role, CheckImport: func(_ context.Context, _, deleted []string) error {
					imported = true
					// Final-manifest absence reaches the policy before any worktree mutation.
					if !reflect.DeepEqual(deleted, []string{"deleted"}) {
						t.Fatalf("deleted: %v", deleted)
					}
					if _, err := os.Stat(filepath.Join(dir, "deleted")); err != nil {
						t.Fatalf("deleted before import check: %v", err)
					}
					calls, _ := os.ReadFile(filepath.Join(root, "calls"))
					lines := string(calls)
					pause, export, termination := strings.Index(lines, "pause "), strings.Index(lines, "cp "+strings.Repeat("a", 64)+":/workspace/. -"), strings.Index(lines, "rm --force")
					if pause < 0 || export < pause || termination < export {
						t.Fatalf("unsafe ordering: %s", lines)
					}
					if failure == "import" {
						return errors.New("import rejected")
					}
					return nil
				}}
				err := RunWorker(ctx, dir, "worker-payload", nil, opts, nil, io.Discard, io.Discard)
				data, _ := os.ReadFile(filepath.Join(dir, "source"))
				if failure == "" {
					if err != nil || !imported || string(data) != "saved" {
						t.Fatalf("lost paused output: %s %v", data, err)
					}
				} else if err == nil || errors.Is(err, claimpause.ErrBudget) || string(data) != "before" {
					t.Fatalf("failed cleanup imported: %s %v", data, err)
				}
				_, statErr := os.Stat(filepath.Join(dir, "deleted"))
				if (failure == "") != errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("deletion imported=%v with failure %q", statErr, failure)
				}
			})
		}
	}
}

// writeFakeExecutable holds the fork lock while the file is open for writing, so a
// parallel test's fork cannot inherit the descriptor and make exec fail with
// "text file busy" (golang/go#22315).
func writeFakeExecutable(path, script string) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, []byte(script), 0700)
}
