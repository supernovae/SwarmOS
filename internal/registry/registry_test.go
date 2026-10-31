package registry

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initTestGitRepo(t *testing.T, remoteURL string) string {
	t.Helper()
	tmp := t.TempDir()
	if out, err := gitfixture.Command(t, "", "init", "-b", "main", tmp).CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v (%s)", err, string(out))
	}
	if remoteURL != "" {
		if out, err := gitfixture.Command(t, "", "-C", tmp, "remote", "add", "origin", remoteURL).CombinedOutput(); err != nil {
			t.Fatalf("git remote add failed: %v (%s)", err, string(out))
		}
	}
	return tmp
}

func TestRegistryAddListRemoveAndSafetyChecks(t *testing.T) {
	regFile := filepath.Join(t.TempDir(), "repos.yaml")
	reg := New(regFile)

	repoA := initTestGitRepo(t, "https://github.com/supernovae/SwarmOS.git")
	repoB := initTestGitRepo(t, "https://github.com/supernovae/OtherProject.git")

	// 1. Auto-detect owner/repo from git remote
	entryA, err := reg.Add("", repoA, "")
	if err != nil {
		t.Fatalf("Add repoA failed: %v", err)
	}
	if entryA.Repo != "supernovae/SwarmOS" || entryA.Backend != "github" {
		t.Fatalf("unexpected entryA: %+v", entryA)
	}

	// 2. Reject mismatched owner/repo when pointing at repoB
	_, err = reg.Add("supernovae/WrongRepo", repoB, "github")
	if err == nil || !strings.Contains(err.Error(), "repository mismatch") {
		t.Fatalf("expected repository mismatch error, got: %v", err)
	}

	// 3. Register repoB with matching slug
	if _, err := reg.Add("supernovae/OtherProject", repoB, "github"); err != nil {
		t.Fatalf("Add repoB failed: %v", err)
	}

	list, err := reg.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("expected 2 registered repos, got %d (%v)", len(list), err)
	}

	got, ok, err := reg.Get("supernovae/swarmos")
	if err != nil || !ok || got.Repo != "supernovae/SwarmOS" {
		t.Fatalf("Get case-insensitive lookup failed: ok=%v err=%v got=%+v", ok, err, got)
	}

	if err := reg.Remove("supernovae/OtherProject"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	listAfter, _ := reg.List()
	if len(listAfter) != 1 {
		t.Fatalf("expected 1 repo after remove, got %d", len(listAfter))
	}
}

func TestReadSnapshotDoesNotInitialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "repos.yaml")
	got, err := New(path).ReadSnapshot(context.Background(), 256, func(int64) error { return nil })
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("created registry directory: %v", err)
	}
}
func TestReadSnapshotBoundedAndNoLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.yaml")
	data := "version: 1\nrepos:\n  local/test:\n    repo: local/test\n    path: /existing/repository\n    backend: local\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	got, err := New(path).ReadSnapshot(context.Background(), 256, func(n int64) error {
		calls++
		if n <= 0 {
			t.Fatal(n)
		}
		return nil
	})
	if err != nil || len(got) != 1 || calls < 2 {
		t.Fatalf("%v %v calls=%d", got, err, calls)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock created: %v", err)
	}
	sentinel := errors.New("budget refused")
	_, err = New(path).ReadSnapshot(context.Background(), 256, func(int64) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Repeat("x", 256<<10+1), "version: 1\nrepos: &recursive\n  x: *recursive\n"} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(path).ReadSnapshot(context.Background(), 256, func(int64) error { return nil }); err == nil {
			t.Fatal("accepted oversized or alias input")
		}
	}
}

func TestReadSnapshotStreamsPartialEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.yaml")
	data := "version: 1\nrepos:\n"
	for i := 0; i < 4; i++ {
		data += fmt.Sprintf("    local/repo%d:\n        repo: local/repo%d\n        path: /repo%d\n        backend: local\n", i, i, i)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	charged := int64(0)
	entries, err := New(path).ReadSnapshot(context.Background(), 2, func(n int64) error { charged += n; return nil })
	if err == nil || len(entries) != 2 {
		t.Fatalf("partial=%v error=%v", entries, err)
	}
	if charged > 128*int64(len(data))+32768 {
		t.Fatal("unexpected parser budget", charged)
	}
	for _, entry := range entries {
		if entry.Backend != "local" {
			t.Fatal(entry)
		}
	}
}
