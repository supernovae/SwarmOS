package registry

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
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
