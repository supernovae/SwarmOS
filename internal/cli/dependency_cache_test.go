package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		!strings.Contains(detail, "python: per-repository") || !strings.Contains(detail, "sharing host caches (no supported lockfile)") {
		t.Fatalf("default dependency-caches check = %v", check)
	}
	_, shared, ok := strings.Cut(detail, "sharing host caches read-only")
	if !ok {
		t.Fatalf("doctor does not name ecosystems sharing host caches: %q", detail)
	}
	for _, name := range []string{"node", "dotnet", "maven", "gradle", "c-cpp"} {
		if !strings.Contains(shared, name) {
			t.Errorf("doctor does not name %s as sharing host caches: %q", name, detail)
		}
	}
	shared, _, _ = strings.Cut(shared, ";")
	for _, name := range []string{"go", "rust", "python"} {
		if strings.Contains(shared, name) {
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
