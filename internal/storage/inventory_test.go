package storage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	return Options{RegistryPath: filepath.Join(root, "registry", "repos.yaml"), CacheRoot: filepath.Join(root, "cache", "swarm"), ScratchRoot: filepath.Join(root, "tmp"), Repository: "absent/repo"}
}
func put(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestInventoryClassesAndUnknowns(t *testing.T) {
	opts := fixture(t)
	for _, class := range []string{"agent-build", "validate-build", "agent-deps", "validate-deps", "go-build"} {
		put(t, filepath.Join(opts.CacheRoot, class, "orphan", "go", "data"), "hello")
	}
	for _, prefix := range []string{"swarm-worker-", "swarm-native-", "swarm-prefetch-", "swarm-review-", "swarm-check-home-"} {
		put(t, filepath.Join(opts.ScratchRoot, prefix+"fixture", "data"), "world")
	}
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, r := range out.Resources {
		classes[r.Class] = true
		if r.Active != nil || r.LastUse != nil || r.Eligibility != "unknown" {
			t.Fatalf("unsupported certainty: %+v", r)
		}
		if r.LogicalBytes == nil || *r.LogicalBytes != 5 {
			t.Fatalf("usage: %+v", r)
		}
	}
	for _, c := range []string{"agent-build", "validate-build", "legacy-go-build", "agent-deps", "validate-deps", "sandbox-scratch", "native-scratch", "prefetch-scratch", "review-scratch", "validation-scratch"} {
		if !classes[c] {
			t.Errorf("missing %s", c)
		}
	}
	if _, err := os.Stat(opts.RegistryPath); !os.IsNotExist(err) {
		t.Fatalf("registry created: %v", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"active":null`) || !strings.Contains(string(b), `"last_use":null`) {
		t.Fatal(string(b))
	}
}
func TestHardlinksAndIdentityExhaustion(t *testing.T) {
	opts := fixture(t)
	a := filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go", "a")
	b := filepath.Join(opts.CacheRoot, "validate-build", "orphan", "go", "b")
	put(t, a, strings.Repeat("a", 4096))
	if err := os.MkdirAll(filepath.Dir(b), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Skip(err)
	}
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 2 {
		t.Fatal(out.Resources)
	}
	var perResource int64
	for _, r := range out.Resources {
		if r.LogicalBytes == nil || *r.LogicalBytes != 4096 || r.AllocatedBytes == nil {
			t.Fatalf("hardlink usage missing: %+v", r)
		}
		perResource += *r.AllocatedBytes
	}
	if out.ObservedAllocatedBytes >= perResource {
		t.Fatalf("not deduplicated: %d >= %d", out.ObservedAllocatedBytes, perResource)
	}
	opts.Limits = DefaultLimits()
	opts.Limits.Identities = 1
	out, err = Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Complete || out.UniqueAllocatedBytes != nil || out.Counters.Identities != 1 {
		t.Fatalf("exhaustion: %+v", out)
	}
}
func TestLimitsAndCancellation(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			opts := fixture(t)
			for _, name := range []string{"a", "b", "c"} {
				put(t, filepath.Join(opts.CacheRoot, "agent-build", "orphan", name, "data"), "data")
			}
			opts.Limits = DefaultLimits()
			opts.Limits.Records = limit
			out, err := Scan(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if out.Counters.Records > limit || len(out.Resources) > limit {
				t.Fatal(out)
			}
		})
	}
	opts := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := Scan(ctx, opts)
	if err != context.Canceled || out.Complete {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	opts.Limits = DefaultLimits()
	opts.Limits.Entries = 1
	put(t, filepath.Join(opts.CacheRoot, "agent-build", "key", "go", "data"), "x")
	out, err = Scan(context.Background(), opts)
	if err != nil || out.Complete || out.Counters.Entries > 1 {
		t.Fatalf("entries: %+v %v", out, err)
	}
}
func TestSymlinkAndMissingUsage(t *testing.T) {
	opts := fixture(t)
	outside := t.TempDir()
	put(t, filepath.Join(outside, "secret"), "outside")
	p := filepath.Join(opts.CacheRoot, "go-build", "orphan")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Skip(err)
	}
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 1 || out.Resources[0].LogicalBytes != nil || out.Complete {
		t.Fatalf("unsafe path: %+v", out)
	}
}
func TestReadOnlyGitInventory(t *testing.T) {
	opts := fixture(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return string(out)
	}
	git("init", "-q")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "init")
	git("branch", "swarm/issue-1-fixture")
	path := filepath.Join(root, ".swarm", "worktrees", "issue-1")
	git("worktree", "add", "-q", path, "swarm/issue-1-fixture")
	git("branch", "swarm/pr-2")
	before := git("show-ref")
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	opts.Repository = ""
	opts.SelectedRoot = root
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, r := range out.Resources {
		count[r.Class]++
	}
	if count["shared-git"] != 1 || count["issue-worktree"] != 1 || count["code-branch"] != 2 {
		t.Fatalf("classes: %v %+v", count, out)
	}
	if got := git("show-ref"); got != before {
		t.Fatal("refs changed")
	}
	after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if string(after) != string(index) {
		t.Fatal("index changed")
	}
}

func TestBudgetAdmissionAndCappedOutput(t *testing.T) {
	b := Budget{Limits: Limits{Bytes: 10}}
	if err := b.Charge(9); err != nil {
		t.Fatal(err)
	}
	if err := b.Charge(1); err != nil {
		t.Fatal(err)
	}
	if err := b.Charge(1); err != ErrLimit || b.Counters.Bytes != 10 {
		t.Fatalf("%+v %v", b, err)
	}
	canceled := false
	w := &cappedOutput{remaining: 3, cancel: func() { canceled = true }}
	if _, err := w.Write([]byte("four")); err != ErrLimit || !canceled || w.buffer.Len() != 0 {
		t.Fatal("input materialized before admission")
	}
}
func TestMidDirectoryCancellation(t *testing.T) {
	opts := fixture(t)
	for _, n := range []string{"a", "b", "c"} {
		put(t, filepath.Join(opts.ScratchRoot, n), "x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := scanner{ctx: ctx, b: Budget{Limits: DefaultLimits()}, out: Snapshot{Complete: true}}
	visits := 0
	s.directory(opts.ScratchRoot, func(os.DirEntry) { visits++; cancel() })
	if visits != 1 || s.out.Complete {
		t.Fatalf("visits=%d complete=%v", visits, s.out.Complete)
	}
}
func TestReadOnlyMetadataSnapshot(t *testing.T) {
	opts := fixture(t)
	put(t, filepath.Join(opts.CacheRoot, "agent-build", "orphan", "go", "data"), "unchanged")
	put(t, filepath.Join(opts.ScratchRoot, "swarm-review-fixture", "data"), "unchanged")
	type metadata struct {
		mode     os.FileMode
		size     int64
		modified string
		contents string
	}
	snap := func() map[string]metadata {
		result := map[string]metadata{}
		err := filepath.WalkDir(filepath.Dir(opts.CacheRoot), func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			v := metadata{mode: info.Mode(), size: info.Size(), modified: info.ModTime().String()}
			if info.Mode().IsRegular() {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				v.contents = string(b)
			}
			result[path] = v
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snap()
	if _, err := Scan(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	after := snap()
	if len(before) != len(after) {
		t.Fatal("names changed")
	}
	for name, info := range before {
		if after[name] != info {
			t.Fatalf("changed %s", name)
		}
	}
}
