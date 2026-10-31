package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/supernovae/swarmos/internal/storageuse"
)

func TestProvenLegacyAndDurableUseEvidence(t *testing.T) {
	opts := fixture(t)
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	opts.Repository = ""
	opts.SelectedRoot = repo
	common := filepath.Join(repo, ".git")
	h := sha256.Sum256([]byte(common))
	key := hex.EncodeToString(h[:16])
	legacy := filepath.Join(opts.CacheRoot, "go-build", key)
	put(t, filepath.Join(legacy, "data"), "legacy")
	path := filepath.Join(opts.CacheRoot, "agent-build", key, "go")
	control := filepath.Join(t.TempDir(), "control")
	manager, err := storageuse.Open(control)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	resource := storageuse.Resource{Repository: key, Class: "agent-build", Identity: "cache:" + path, Location: path}
	use, err := manager.AcquireUse(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, "data"), "cache")
	if err := use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	opts.UseRoot = control
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range out.Resources {
		if r.Path == legacy && r.Repository == "" {
			t.Fatal("proven legacy not associated")
		}
		if r.Path == path {
			found = true
			if r.Eligibility != "eligible" || r.Active == nil || *r.Active || r.LastUse == nil {
				t.Fatalf("positive evidence: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("cache missing")
	}
	// Complete use evidence cannot excuse a substituted filesystem entry.
	backup := path + "-saved"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	unsafe, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range unsafe.Resources {
		if r.Path == path && r.Eligibility != "unknown" {
			t.Fatalf("use evidence excused symlink: %+v", r)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AcquireUse(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	out, err = Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Resources {
		if r.Path == path && (r.Eligibility != "blocked" || r.Active == nil || !*r.Active) {
			t.Fatalf("unresolved use: %+v", r)
		}
	}
	if err := os.Chmod(legacy, 0777); err != nil {
		t.Fatal(err)
	}
	out, err = Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Resources {
		if r.Path == legacy && r.Repository != "" {
			t.Fatal("unsafe legacy claimed")
		}
	}
}

func TestPrivateLayoutRejectsWritableAnchor(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "go-build", "key"), 0700); err != nil {
		t.Fatal(err)
	}
	if !privateLayout(base, filepath.Join("go-build", "key")) {
		t.Fatal("private layout rejected")
	}
	if err := os.Chmod(base, 0777); err != nil {
		t.Fatal(err)
	}
	if privateLayout(base, filepath.Join("go-build", "key")) {
		t.Fatal("writable anchor accepted")
	}
}

func TestIndexedManagedScratchUseEvidence(t *testing.T) {
	opts := fixture(t)
	opts.Repository = ""
	control := filepath.Join(t.TempDir(), "control")
	manager, err := storageuse.Open(control)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	path := filepath.Join(opts.ScratchRoot, "swarm-check-home-indexed")
	resource := storageuse.ResourceFor("host-unknown", "validation-scratch", path)
	use, err := manager.AcquireUse(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, "data"), "scratch")
	opts.UseRoot = control
	before, err := os.ReadDir(control)
	if err != nil {
		t.Fatal(err)
	}
	check := func(active bool) {
		t.Helper()
		snapshot, err := Scan(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snapshot.Resources {
			if r.Path == path {
				if r.Class != "validation-scratch" || r.Active == nil || *r.Active != active || r.LastUse == nil {
					t.Fatalf("scratch evidence: %+v", r)
				}
				if r.Repository != "" || r.Eligibility == "eligible" {
					t.Fatalf("host scope inferred repository eligibility: %+v", r)
				}
				return
			}
		}
		t.Fatal("indexed scratch not observed")
	}
	check(true)
	if err := use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	check(false)
	after, err := os.ReadDir(control)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("report created control entries")
	}
}

func TestIndexedRustPrefetchScratchMatchesDiscovery(t *testing.T) {
	opts := fixture(t)
	opts.Repository = ""
	opts.UseRoot = filepath.Join(t.TempDir(), "control")
	manager, err := storageuse.Open(opts.UseRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	key := "0123456789abcdef0123456789abcdef"
	path := filepath.Join(opts.CacheRoot, "agent-deps", key, ".prefetch-rust-fixture")
	use, err := manager.AcquireUse(context.Background(), storageuse.ResourceFor(key, "prefetch-scratch", path))
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, "consumer"), "active")
	out, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range out.Resources {
		if r.Path == path {
			count++
			if r.Identity != "scratch:"+path || r.Active == nil || !*r.Active {
				t.Fatalf("prefetch evidence diverged: %+v", r)
			}
		}
	}
	if count != 1 {
		t.Fatalf("prefetch resource appears %d times", count)
	}
	if err := use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverScopesRegisteredOnlyAndConflicts(t *testing.T) {
	ctx := context.Background()
	opts := fixture(t)
	opts.Repository = ""
	scopes, reasons, err := DiscoverScopes(ctx, opts, true)
	if err != nil || len(scopes) != 0 || len(reasons) != 0 {
		t.Fatalf("empty registry %v %v %v", scopes, reasons, err)
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v: %s %v", args, out, e)
		}
	}
	git("init", "-q")
	put(t, filepath.Join(repo, "file"), "x")
	git("add", "file")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	git("worktree", "add", "-qb", "linked", linked)
	// ReadSnapshot preserves distinct records, enabling independent common-dir
	// identity checks even when one registration lies about its display name.
	put(t, opts.RegistryPath, fmt.Sprintf("version: 1\nrepos:\n  a/repo:\n    repo: a/repo\n    path: %s\n    backend: local\n  b/repo:\n    repo: b/repo\n    path: %s\n    backend: local\n", repo, linked))
	scopes, reasons, err = DiscoverScopes(ctx, opts, true)
	if err != nil || len(scopes) != 0 || len(reasons) == 0 {
		t.Fatalf("conflicting identity accepted: %v %v %v", scopes, reasons, err)
	}
	put(t, opts.RegistryPath, fmt.Sprintf("version: 1\nrepos:\n  a/repo:\n    repo: a/repo\n    path: %s\n    backend: local\n", linked))
	scopes, reasons, err = DiscoverScopes(ctx, opts, true)
	if err != nil || len(scopes) != 1 || scopes[0].Root != repo || len(reasons) != 0 {
		t.Fatalf("linked worktree: %v %v %v", scopes, reasons, err)
	}
	opts.SelectedRoot = linked
	scopes, reasons, err = DiscoverScopes(ctx, opts, false)
	if err != nil || len(scopes) != 1 || scopes[0].Backend != "local" || len(reasons) != 0 {
		t.Fatalf("registered linked default: %v %v %v", scopes, reasons, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".swarm")); !os.IsNotExist(err) {
		t.Fatal("discovery created authority", err)
	}
}
