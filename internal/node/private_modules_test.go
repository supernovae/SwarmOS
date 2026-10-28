package node

import (
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/sandbox"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return dir
}

func TestPrivateGoModulesResolvesRegisteredRepository(t *testing.T) {
	repoA, repoB, unregistered := gitRepo(t), gitRepo(t), gitRepo(t)
	worktree := filepath.Join(repoA, ".swarm", "worktrees", "issue-7")
	if out, err := gitfixture.Command(t, "", "-C", repoA, "worktree", "add", "-q", "--detach", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v (%s)", err, out)
	}
	registryPath := filepath.Join(t.TempDir(), "repos.yaml")
	reg := registry.New(registryPath)
	for repo, path := range map[string]string{"acme/a": repoA, "acme/b": repoB} {
		if _, err := reg.Add(repo, path, "local"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, profile, worktree string
		want                    sandbox.PrivateGo
		warn                    bool
		wantErr                 bool
	}{
		{"repository root", "private_go_modules:\n  acme/a: [example.com/priv]\n", repoA, sandbox.PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/priv"}}, false, false},
		{"issue worktree", "private_go_modules:\n  acme/a: [example.com/priv, git.example.org/team/*]\n", worktree, sandbox.PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/priv", "git.example.org/team/*"}}, false, false},
		{"case-insensitive key", "private_go_modules:\n  ACME/A: [example.com/priv]\n", repoA, sandbox.PrivateGo{Repository: "acme/a", Patterns: []string{"example.com/priv"}}, false, false},
		{"other repository's entry", "private_go_modules:\n  acme/b: [example.com/priv]\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, false, false},
		{"unregistered worktree", "private_go_modules:\n  acme/a: [example.com/priv]\n", unregistered, sandbox.PrivateGo{}, false, false},
		{"no profile entry", "node_id: n\n", repoA, sandbox.PrivateGo{}, false, false},
		{"missing profile", "", repoA, sandbox.PrivateGo{}, false, false},
		{"malformed profile", "private_go_modules: [\n", repoA, sandbox.PrivateGo{}, false, true},
		{"invalid slug", "private_go_modules:\n  not-a-slug: [example.com/priv]\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"match-all pattern", "private_go_modules:\n  acme/a: ['*']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"wildcard host", "private_go_modules:\n  acme/a: ['*.com/x']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"comma list", "private_go_modules:\n  acme/a: ['a.com/x,b.com/y']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"empty pattern", "private_go_modules:\n  acme/a: ['']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"host without dot", "private_go_modules:\n  acme/a: [localhost/x]\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"one bad pattern drops the entry", "private_go_modules:\n  acme/a: [example.com/priv, '*']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
		{"bad spelling drops a valid one", "private_go_modules:\n  acme/a: [example.com/priv]\n  ACME/A: ['*']\n", repoA, sandbox.PrivateGo{Repository: "acme/a"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "node.yaml")
			if tc.profile != "" {
				if err := os.WriteFile(profile, []byte(tc.profile), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			policy, warnings, err := LoadDependencyPolicy(profile, registryPath, tc.worktree)
			if (err != nil) != tc.wantErr || (len(warnings) > 0) != tc.warn {
				t.Fatalf("LoadDependencyPolicy = %+v, warnings %q, err %v", policy, warnings, err)
			}
			if policy.Mode != sandbox.PerRepository || policy.PrivateGo.Repository != tc.want.Repository || !slices.Equal(policy.PrivateGo.Patterns, tc.want.Patterns) {
				t.Fatalf("policy = %+v, want %+v", policy, tc.want)
			}
			if got := DependencyPolicy(profile, registryPath, tc.worktree); got.PrivateGo.Repository != tc.want.Repository || !slices.Equal(got.PrivateGo.Patterns, tc.want.Patterns) {
				t.Fatalf("DependencyPolicy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPrivateGoEntriesMarkRegistration(t *testing.T) {
	repoA := gitRepo(t)
	registryPath := filepath.Join(t.TempDir(), "repos.yaml")
	if _, err := registry.New(registryPath).Add("acme/a", repoA, "local"); err != nil {
		t.Fatal(err)
	}
	prof, err := readProfileBody(t, "private_go_modules:\n  acme/z: [example.com/z]\n  acme/a: [example.com/a]\n  acme/bad: ['*']\n")
	if err != nil {
		t.Fatal(err)
	}
	entries := PrivateGoEntries(prof, registryPath)
	if len(entries) != 3 || entries[0].Repository != "acme/a" || !entries[0].Registered || entries[0].Problem != "" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[1].Repository != "acme/bad" || entries[1].Problem == "" || entries[2].Registered {
		t.Fatalf("entries = %+v", entries)
	}
	// Reading a missing registry never creates it.
	missing := filepath.Join(t.TempDir(), "absent", "repos.yaml")
	if got := PrivateGoEntries(prof, missing); got[0].Registered {
		t.Fatalf("entries = %+v", got)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("PrivateGoEntries created %s", filepath.Dir(missing))
	}
}

func readProfileBody(t *testing.T, body string) (*contract.NodeProfile, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return readProfile(path)
}

func TestValidatePrivateGoPattern(t *testing.T) {
	for pattern, ok := range map[string]bool{
		"example.com/priv": true, "github.com/acme/*": true, "*.corp.example.com": false, "example.com": true,
		"*": false, "": false, "a.com/x b.com/y": false, "a.com/[": false, "corp/x": false,
	} {
		if err := ValidatePrivateGoPattern(pattern); (err == nil) != ok {
			t.Errorf("ValidatePrivateGoPattern(%q) = %v, want ok %t", pattern, err, ok)
		}
	}
}
