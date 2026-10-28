package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/registry"
)

func writeNodeProfile(t *testing.T, f *cliFixture, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setenv("SWARM_NODE_CONFIG", path)
}

func doctorDependencyCheck(t *testing.T, f *cliFixture) map[string]any {
	t.Helper()
	out, _ := f.run("repo", "doctor", "--json")
	var doctor struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &doctor); err != nil {
		t.Fatalf("doctor JSON: %v\n%s", err, out)
	}
	for _, check := range doctor.Checks {
		if check["name"] == "dependency-caches" {
			return check
		}
	}
	t.Fatalf("doctor has no dependency-caches check: %v", doctor.Checks)
	return nil
}

func TestDoctorReportsDependencyCacheModes(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	writeNodeProfile(t, f, "node_id: n\n")
	check := doctorDependencyCheck(t, f)
	detail, _ := check["detail"].(string)
	if check["status"] != "OK" || !strings.Contains(detail, "go: per-repository") || !strings.Contains(detail, "rust: per-repository") ||
		!strings.Contains(detail, "python: per-repository") || !strings.Contains(detail, "node: per-repository") ||
		!strings.Contains(detail, "sharing host caches (no supported lockfile)") {
		t.Fatalf("default dependency-caches check = %v", check)
	}
	_, shared, ok := strings.Cut(detail, "sharing host caches read-only")
	if !ok {
		t.Fatalf("doctor does not name ecosystems sharing host caches: %q", detail)
	}
	for _, name := range []string{"node-browsers", "dotnet", "maven", "gradle", "c-cpp"} {
		if !strings.Contains(shared, name) {
			t.Errorf("doctor does not name %s as sharing host caches: %q", name, detail)
		}
	}
	shared, _, _ = strings.Cut(shared, ";")
	// Match whole names, so node-browsers does not count as node.
	names := map[string]bool{}
	for _, name := range strings.FieldsFunc(shared, func(r rune) bool { return strings.ContainsRune(",:;() ", r) }) {
		names[name] = true
	}
	for _, name := range []string{"go", "rust", "python", "node"} {
		if names[name] {
			t.Errorf("%s is listed as sharing host caches by default: %q", name, detail)
		}
	}
}

func TestNodeStatusReportsDependencyCacheMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, profile, goMode, doctorStatus string
	}{
		{"default", "node_id: n\n", "per-repository", "OK"},
		{"opted in", "node_id: n\nshare_host_dependency_caches: true\n", "host-shared (opt-in)", "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCLIFixture(t)
			writeNodeProfile(t, f, tc.profile)
			if out := f.must(t, "node", "status"); !strings.Contains(out, "Dependency caches: go: "+tc.goMode) {
				t.Fatalf("node status does not report go %s:\n%s", tc.goMode, out)
			}
			var status struct {
				DependencyCaches []struct{ Name, Mode string } `json:"dependency_caches"`
			}
			out := f.must(t, "node", "status", "--json")
			if err := json.Unmarshal([]byte(out), &status); err != nil {
				t.Fatal(err)
			}
			modes := map[string]string{}
			for _, eco := range status.DependencyCaches {
				modes[eco.Name] = eco.Mode
			}
			if modes["go"] != tc.goMode || !strings.HasPrefix(modes["maven"], "host-shared (no fetch-only prefetch") {
				t.Fatalf("node status --json dependency_caches = %v", modes)
			}
			check := doctorDependencyCheck(t, f)
			if detail, _ := check["detail"].(string); check["status"] != tc.doctorStatus || !strings.Contains(detail, "go: "+tc.goMode) {
				t.Fatalf("doctor dependency-caches = %v", check)
			}
		})
	}
}

// registerFixture registers f's repository as acme/a in a fresh registry.
func registerFixture(t *testing.T, f *cliFixture) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repos.yaml")
	if _, err := registry.New(path).Add("acme/a", f.root, "local"); err != nil {
		t.Fatal(err)
	}
	f.setenv("SWARM_REGISTRY_PATH", path)
}

func TestNodeStatusReportsPrivateGoModules(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	registerFixture(t, f)
	writeNodeProfile(t, f, "node_id: n\nprivate_go_modules:\n  acme/a: [example.com/priv]\n  acme/other: [example.com/other]\n")
	out := f.must(t, "node", "status")
	if !strings.Contains(out, "Private Go modules: acme/a: example.com/priv; acme/other (unregistered): example.com/other") {
		t.Fatalf("node status does not report private Go modules:\n%s", out)
	}
	var status struct {
		PrivateGoModules []struct {
			Repository string   `json:"repository"`
			Patterns   []string `json:"patterns"`
			Registered bool     `json:"registered"`
		} `json:"private_go_modules"`
	}
	out = f.must(t, "node", "status", "--json")
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	got := status.PrivateGoModules
	if len(got) != 2 || got[0].Repository != "acme/a" || !got[0].Registered || strings.Join(got[0].Patterns, ",") != "example.com/priv" ||
		got[1].Repository != "acme/other" || got[1].Registered {
		t.Fatalf("node status --json private_go_modules = %+v", got)
	}

	writeNodeProfile(t, f, "node_id: n\n")
	if out := f.must(t, "node", "status"); !strings.Contains(out, "Private Go modules: none") {
		t.Fatalf("node status without private modules:\n%s", out)
	}
	if out := f.must(t, "node", "status", "--json"); !strings.Contains(out, `"private_go_modules":[]`) {
		t.Fatalf("node status --json without private modules: %s", out)
	}
}

func TestDoctorReportsPrivateGoModules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, profile, status string
		register              bool
		want                  []string
	}{
		{"allowed", "node_id: n\nprivate_go_modules:\n  acme/a: [example.com/priv, git.example.org/team]\n", "OK", true,
			[]string{"go: per-repository", "private Go modules for acme/a: example.com/priv, git.example.org/team", "withdrawing an allowance"}},
		{"other repository", "node_id: n\nprivate_go_modules:\n  acme/b: [example.com/priv]\n", "WARN", true,
			[]string{"private Go modules for acme/a: none", `private_go_modules["acme/b"] names an unregistered repository`}},
		{"invalid entry", "node_id: n\nprivate_go_modules:\n  acme/a: ['*']\n", "WARN", true,
			[]string{"private Go modules for acme/a: none", `private_go_modules["acme/a"] allows nothing`}},
		{"unregistered", "node_id: n\n", "OK", false, []string{"private Go modules: none (repository not registered)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCLIFixture(t)
			if tc.register {
				registerFixture(t, f)
			}
			writeNodeProfile(t, f, tc.profile)
			check := doctorDependencyCheck(t, f)
			detail, _ := check["detail"].(string)
			if check["status"] != tc.status {
				t.Fatalf("doctor dependency-caches = %v, want %s", check, tc.status)
			}
			for _, want := range tc.want {
				if !strings.Contains(detail, want) {
					t.Errorf("doctor detail %q does not contain %q", detail, want)
				}
			}
		})
	}
}
