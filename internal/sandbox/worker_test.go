package sandbox

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/safefs"
)

func writeWorkerFixture(t *testing.T, root, name, content string) {
	t.Helper()
	if err := safefs.WriteFile(root, filepath.FromSlash(name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerArchiveRoundTripAndImport(t *testing.T) {
	original, scratch, result := t.TempDir(), t.TempDir(), t.TempDir()
	writeWorkerFixture(t, original, "kept.txt", "same")
	writeWorkerFixture(t, original, "changed.txt", "old")
	writeWorkerFixture(t, original, "deleted.txt", "remove")
	writeWorkerFixture(t, original, ".git/config", "coordinator secret")
	writeWorkerFixture(t, original, ".swarm/coordination/state", "authority")
	archive := filepath.Join(scratch, "input.tar")
	initial, err := packWorker(context.Background(), original, archive, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := initial[".git/config"]; ok {
		t.Fatal("Git authority sent to worker")
	}
	if _, ok := initial[".swarm/coordination/state"]; ok {
		t.Fatal("coordinator state sent to worker")
	}
	final, err := unpackWorker(context.Background(), archive, result)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 3 {
		t.Fatalf("unexpected manifest: %#v", final)
	}
	writeWorkerFixture(t, result, "changed.txt", "new")
	writeWorkerFixture(t, result, "added.txt", "added")
	if err := os.Remove(filepath.Join(result, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	resultArchive := filepath.Join(scratch, "result.tar")
	final, err = packWorker(context.Background(), result, resultArchive, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := importWorker(context.Background(), root, result, initial, final); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"kept.txt": "same", "changed.txt": "new", "added.txt": "added", ".git/config": "coordinator secret", ".swarm/coordination/state": "authority"} {
		got, err := os.ReadFile(filepath.Join(original, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(original, "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file retained: %v", err)
	}
}

func TestWorkerImportRejectsConcurrentChanges(t *testing.T) {
	for _, kind := range []string{"changed-original", "new-conflict"} {
		t.Run(kind, func(t *testing.T) {
			source, result, scratch := t.TempDir(), t.TempDir(), t.TempDir()
			writeWorkerFixture(t, source, "original", "before")
			initial, err := packWorker(context.Background(), source, filepath.Join(scratch, "input.tar"), nil, false)
			if err != nil {
				t.Fatal(err)
			}
			writeWorkerFixture(t, result, "original", "worker edit")
			writeWorkerFixture(t, result, "new", "worker new")
			final, err := packWorker(context.Background(), result, filepath.Join(scratch, "result.tar"), nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "changed-original" {
				writeWorkerFixture(t, source, "original", "human edit")
			} else {
				writeWorkerFixture(t, source, "new", "human new")
			}
			root, err := safefs.Open(source)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err := importWorker(context.Background(), root, result, initial, final); err == nil {
				t.Fatal("conflicting worktree was overwritten")
			}
			got, err := os.ReadFile(filepath.Join(source, "original"))
			if err != nil || string(got) == "worker edit" {
				t.Fatalf("partial import before conflict check: %q %v", got, err)
			}
		})
	}
}

func TestWorkerRejectsHostileArchives(t *testing.T) {
	cases := []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg}}},
		{"absolute", []*tar.Header{{Name: "/outside", Typeflag: tar.TypeReg}}},
		{"windows", []*tar.Header{{Name: "C:\\outside", Typeflag: tar.TypeReg}}},
		{"git", []*tar.Header{{Name: ".git/config", Typeflag: tar.TypeReg}}},
		{"git-case", []*tar.Header{{Name: ".GIT/config", Typeflag: tar.TypeReg}}},
		{"git-hfs-alias", []*tar.Header{{Name: ".g\u200cit/config", Typeflag: tar.TypeReg}}},
		{"state-hfs-alias", []*tar.Header{{Name: ".swarm/coor\ufeffdination/x", Typeflag: tar.TypeReg}}},
		{"git-alias", []*tar.Header{{Name: ".git. /config", Typeflag: tar.TypeReg}}},
		{"ads", []*tar.Header{{Name: "file:secret", Typeflag: tar.TypeReg}}},
		{"device", []*tar.Header{{Name: "CON.txt", Typeflag: tar.TypeReg}}},
		{"case-collision", []*tar.Header{{Name: "File", Typeflag: tar.TypeReg}, {Name: "file", Typeflag: tar.TypeReg}}},
		{"state", []*tar.Header{{Name: ".swarm/coordination/x", Typeflag: tar.TypeReg}}},
		{"symlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside"}}},
		{"hardlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeLink, Linkname: "target"}}},
		{"fifo", []*tar.Header{{Name: "fifo", Typeflag: tar.TypeFifo}}},
		{"duplicate", []*tar.Header{{Name: "same", Typeflag: tar.TypeReg}, {Name: "same", Typeflag: tar.TypeReg}}},
		{"oversize", []*tar.Header{{Name: "large", Typeflag: tar.TypeReg, Size: workerFileBytes + 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "archive.tar")
			f, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			tw := tar.NewWriter(f)
			for _, header := range tc.headers {
				if err := tw.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
			}
			// An oversized header is enough: unpack must reject before allocating data.
			_ = tw.Close()
			_ = f.Close()
			if _, err := unpackWorker(context.Background(), archive, t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestWorkerInputRejectsLinksAndCancellation(t *testing.T) {
	root := t.TempDir()
	writeWorkerFixture(t, root, "real", "data")
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := packWorker(context.Background(), root, filepath.Join(t.TempDir(), "input.tar"), nil, false); err == nil {
		t.Fatal("input symlink accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := packWorker(ctx, root, filepath.Join(t.TempDir(), "input.tar"), nil, false); err == nil {
		t.Fatal("cancelled input copy succeeded")
	}
}

func TestWorkerRejectsUnpinnedImageAndDoesNotForwardCredentials(t *testing.T) {
	if _, err := workerRuntime("docker", "image:latest"); err == nil {
		t.Fatal("mutable image accepted")
	}
	if _, err := workerRuntime("sh", "image@sha256:"+strings.Repeat("a", 64)); err == nil {
		t.Fatal("arbitrary host executable accepted as OCI runtime")
	}
	t.Setenv("GH_TOKEN", "coordinator-secret")
	t.Setenv("ANTHROPIC_API_KEY", "model-secret")
	t.Setenv("DOCKER_HOST", "tcp://untrusted.invalid")
	cmd := workerCommand(context.Background(), "docker", "info")
	for _, env := range cmd.Env {
		if strings.HasPrefix(env, "GH_TOKEN=") || strings.HasPrefix(env, "ANTHROPIC_API_KEY=") || strings.HasPrefix(env, "DOCKER_HOST=") {
			t.Fatalf("worker runtime inherited secret/endpoint: %s", strings.SplitN(env, "=", 2)[0])
		}
	}
}

func TestWorkerExportBudget(t *testing.T) {
	w := workerBudgetWriter{writer: io.Discard, remaining: 3}
	if _, err := w.Write([]byte("four")); err == nil {
		t.Fatal("export exceeded budget")
	}
}

func TestWorkerProjectPlanHasDedicatedArtifactChannel(t *testing.T) {
	outputs := workerOutputs(WorkerOptions{Role: "planner", ReadOnly: true, ProjectPlan: true})
	if len(outputs) != 1 || outputs[0] != "PROJECT_PLAN.yaml" {
		t.Fatalf("wrong project planning output channel: %v", outputs)
	}
	if err := RunWorker(t.Context(), t.TempDir(), "unused", nil, WorkerOptions{Role: "developer", ProjectPlan: true}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "read-only planner") {
		t.Fatalf("project output used outside planner: %v", err)
	}
}

func TestWorkerPreflightChecksDaemonAndCleansUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake runtime uses a Unix shell; archive tests cover portable importer")
	}
	for _, kind := range []string{"safe", "volume", "windows-image", "privileged", "cleanup-failure", "execution-cleanup-failure", "tmpfs-size", "tmpfs-owner", "tmpfs-mode", "tmpfs-missing", "tmpfs-extra", "tmpfs-duplicate", "duplicate-mount"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			image := map[string]any{"Os": "linux", "Config": map[string]any{"Volumes": nil}}
			if kind == "volume" {
				image["Config"] = map[string]any{"Volumes": map[string]any{"/persistent": map[string]any{}}}
			}
			if kind == "windows-image" {
				image["Os"] = "windows"
			}
			imageJSON, _ := json.Marshal([]any{image})
			host := map[string]any{"Privileged": false, "NetworkMode": "none", "PidMode": "private", "IpcMode": "private", "CgroupnsMode": "private", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}, "Memory": int64(2 << 30), "MemorySwap": int64(2 << 30), "NanoCpus": int64(2000000000), "PidsLimit": 128, "Tmpfs": map[string]string{"/workspace": "rw,nosuid,nodev,size=512m,mode=0755,uid=0,gid=0", "/output": "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534", "/home/worker": "rw,nosuid,nodev,size=67108864,mode=700,uid=65534,gid=65534", "/tmp": "rw,nosuid,nodev,size=64m,mode=1777,uid=0,gid=0"}}
			if kind == "privileged" {
				host["Privileged"] = true
			}
			tmpfs := host["Tmpfs"].(map[string]string)
			switch kind {
			case "tmpfs-size":
				tmpfs["/workspace"] = strings.Replace(tmpfs["/workspace"], "512m", "2g", 1)
			case "tmpfs-owner":
				tmpfs["/workspace"] = strings.Replace(tmpfs["/workspace"], "uid=0", "uid=65534", 1)
			case "tmpfs-mode":
				tmpfs["/workspace"] = strings.Replace(tmpfs["/workspace"], "0755", "0777", 1)
			case "tmpfs-missing":
				tmpfs["/tmp"] = strings.Replace(tmpfs["/tmp"], "nodev,", "", 1)
			case "tmpfs-extra":
				tmpfs["/tmp"] += ",exec"
			case "tmpfs-duplicate":
				tmpfs["/tmp"] += ",size=64m"
			}
			mounts := []any{}
			if kind == "duplicate-mount" {
				mounts = []any{map[string]string{"Type": "tmpfs", "Destination": "/tmp"}, map[string]string{"Type": "tmpfs", "Destination": "/tmp"}}
			}
			configJSON, _ := json.Marshal([]any{map[string]any{"HostConfig": host, "Mounts": mounts}})
			for name, data := range map[string][]byte{"image.json": imageJSON, "container.json": configJSON} {
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			quoted := "'" + strings.ReplaceAll(root, "'", "'\"'\"'") + "'"
			script := "#!/bin/sh\nroot=" + quoted + "\nprintf '%s\\n' \"$*\" >> \"$root/calls\"\ncase \"$1\" in\n image) cat \"$root/image.json\";;\n inspect) cat \"$root/container.json\";;\n create) printf '%s\\n' '" + strings.Repeat("a", 64) + "';;\n rm) "
			if kind == "cleanup-failure" || kind == "execution-cleanup-failure" {
				script += "printf cleanup-failed >&2; exit 1"
			} else {
				script += ":"
			}
			script += ";;\n"
			if kind == "execution-cleanup-failure" {
				script += " cp) cat >/dev/null;;\n exec) case \"$*\" in *worker-payload*) printf execution-failed >&2; exit 19;; *) :;; esac;;\n"
			}
			script += " *) :;;\nesac\n"
			executable := filepath.Join(root, "docker")
			if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "execution-cleanup-failure" {
				err = RunWorker(context.Background(), t.TempDir(), "worker-payload", nil, WorkerOptions{Runtime: executable, Image: "local/image@sha256:" + strings.Repeat("b", 64), ReadOnly: true, Role: "reviewer"}, nil, io.Discard, io.Discard)
				for _, message := range []string{"isolated worker failed: exit status 19", "cleanup-failed", strings.Repeat("a", 64)} {
					if err == nil || !strings.Contains(err.Error(), message) {
						t.Fatalf("lost simultaneous execution/cleanup error %q: %v", message, err)
					}
				}
			} else {
				err = CheckWorkerRuntime(context.Background(), executable, "local/image@sha256:"+strings.Repeat("b", 64))
			}
			if kind == "safe" && err != nil {
				t.Fatal(err)
			}
			if kind != "safe" && err == nil {
				t.Fatal("unsafe/unclean runtime passed preflight")
			}
			calls, readErr := os.ReadFile(filepath.Join(root, "calls"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if kind == "volume" || kind == "windows-image" {
				if strings.Contains(string(calls), "create ") {
					t.Fatal("created container from unsafe image")
				}
			} else {
				for _, required := range []string{"--pull=never", "--network=none", "--pid=private", "--ipc=private", "--cgroupns=private", "--cap-drop=ALL", "--memory=2g", "--pids-limit=128", "rm --force"} {
					if !strings.Contains(string(calls), required) {
						t.Fatalf("missing enforced operation %s", required)
					}
				}
			}
		})
	}
}

func TestWorkerInputRejectsPortableAliases(t *testing.T) {
	for _, names := range [][]string{{"File", "file"}, {"GIT~1/config"}, {".swarm/LOCAL-~1/state"}} {
		root := t.TempDir()
		for _, name := range names {
			writeWorkerFixture(t, root, name, "data")
		}
		if len(names) == 2 {
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				continue
			} // Already aliased by a case-insensitive filesystem.
		}
		if _, err := packWorker(context.Background(), root, filepath.Join(t.TempDir(), "input.tar"), nil, false); err == nil {
			t.Fatalf("portable alias accepted: %v", names)
		}
	}
}

func TestWorkerTmpfsNormalizedOptions(t *testing.T) {
	for _, options := range []string{"nodev,rw,nosuid,gid=65534,uid=65534,size=64M,mode=0700", "rw,nosuid,nodev,size=67108864,mode=700,uid=065534,gid=65534"} {
		if err := verifyWorkerTmpfs(options, 65534, 0700, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	for _, options := range []string{"rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=0", "rw,nosuid,nodev,size=18446744073709551615g,mode=0700,uid=65534,gid=65534", "rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534,rw"} {
		if err := verifyWorkerTmpfs(options, 65534, 0700, 64<<20); err == nil {
			t.Fatalf("unsafe options accepted: %s", options)
		}
	}
	if err := verifyWorkerTmpfs("rw,nosuid,nodev,size=512m,mode=0755,uid=65534,gid=65534", 65534, 0755, 512<<20); err != nil {
		t.Fatal(err)
	}
	if err := verifyWorkerTmpfs("rw,nosuid,nodev,size=512m,mode=0755,uid=65534,gid=65534", 0, 0755, 512<<20); err == nil {
		t.Fatal("readonly workspace accepts worker ownership")
	}
}

func TestWorkerEmptyEnvironmentNeverInherits(t *testing.T) {
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TEMP", "TMP"} {
		old, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	t.Setenv("GH_TOKEN", "synthetic-coordinator-token")
	cmd := workerCommand(context.Background(), "/unexecuted/docker", "info")
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Fatalf("empty allowlist could inherit environment: %#v", cmd.Env)
	}
}
