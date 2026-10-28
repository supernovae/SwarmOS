package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type permissionWorkflow struct {
	Permissions yaml.Node                `yaml:"permissions"`
	Jobs        map[string]permissionJob `yaml:"jobs"`
}
type permissionJob struct {
	Uses        string    `yaml:"uses"`
	Permissions yaml.Node `yaml:"permissions"`
}

func loadPermissionWorkflows(dir string) (map[string]permissionWorkflow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	workflows := map[string]permissionWorkflow{}
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if entry.IsDir() || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var workflow permissionWorkflow
		if err := yaml.Unmarshal([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n")), &workflow); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		workflows[entry.Name()] = workflow
	}
	return workflows, nil
}

// A job block replaces workflow defaults, including when explicitly empty.
func effectiveWorkflowPermissions(job, workflow yaml.Node) (map[string]int, int, error) {
	node := job
	if node.Kind == 0 {
		node = workflow
	}
	if node.Kind == 0 {
		return nil, 0, fmt.Errorf("effective permissions unavailable; declare workflow or job permissions explicitly")
	}
	levels := map[string]int{"none": 0, "read": 1, "write": 2}
	if node.Kind == yaml.ScalarNode {
		switch node.Value {
		case "read-all":
			return map[string]int{"id-token": 0}, 1, nil
		case "write-all":
			return nil, 2, nil
		default:
			return nil, 0, fmt.Errorf("unsupported permissions %q", node.Value)
		}
	}
	if node.Kind != yaml.MappingNode {
		return nil, 0, fmt.Errorf("permissions must be a map or read-all/write-all")
	}
	grants := map[string]int{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		level, ok := levels[value.Value]
		if key.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode || !ok || (key.Value == "id-token" && level == 1) {
			return nil, 0, fmt.Errorf("invalid permission %s: %s", key.Value, value.Value)
		}
		grants[key.Value] = level
	}
	return grants, 0, nil
}

func checkWorkflowPermissions(workflows map[string]permissionWorkflow) (int, []string) {
	var diagnostics []string
	calls := 0
	for file, workflow := range workflows {
		for name, job := range workflow.Jobs {
			const prefix = "./.github/workflows/"
			if !strings.HasPrefix(job.Uses, prefix) {
				continue
			}
			calls++
			target := strings.TrimPrefix(job.Uses, prefix)
			called, ok := workflows[target]
			caller := file + "/" + name
			if !ok {
				diagnostics = append(diagnostics, caller+": missing local workflow "+target)
				continue
			}
			grants, grantDefault, err := effectiveWorkflowPermissions(job.Permissions, workflow.Permissions)
			if err != nil {
				diagnostics = append(diagnostics, caller+": "+err.Error())
				continue
			}
			for calledName, calledJob := range called.Jobs {
				edge := caller + " -> " + target + "/" + calledName
				requests, requestDefault, err := effectiveWorkflowPermissions(calledJob.Permissions, called.Permissions)
				if err != nil {
					diagnostics = append(diagnostics, edge+": "+err.Error())
					continue
				}
				// '*' compares shorthand defaults; explicit scopes include exceptions such as id-token.
				scopes := map[string]bool{"*": true}
				for scope := range requests {
					scopes[scope] = true
				}
				for scope := range grants {
					scopes[scope] = true
				}
				for scope := range scopes {
					required, ok := requests[scope]
					if !ok {
						required = requestDefault
					}
					available, ok := grants[scope]
					if !ok {
						available = grantDefault
					}
					if required > available {
						levels := []string{"none", "read", "write"}
						diagnostics = append(diagnostics, fmt.Sprintf("%s: %s requires %s, available %s", edge, scope, levels[required], levels[available]))
					}
				}
			}
		}
	}
	sort.Strings(diagnostics)
	return calls, diagnostics
}

func TestLocalReusableWorkflowPermissions(t *testing.T) {
	workflows, err := loadPermissionWorkflows("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	calls, diagnostics := checkWorkflowPermissions(workflows)
	if calls == 0 {
		t.Fatal("no local workflow calls checked")
	}
	if len(diagnostics) > 0 {
		t.Fatal(strings.Join(diagnostics, "\n"))
	}
	t.Logf("checked %d local workflow calls", calls)
}

func TestLocalReusableWorkflowPermissionsRegression(t *testing.T) {
	workflows, err := loadPermissionWorkflows("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	if _, diagnostics := checkWorkflowPermissions(workflows); len(diagnostics) > 0 {
		t.Fatal(diagnostics)
	}
	release := workflows["release.yml"]
	job, ok := release.Jobs["checks"]
	if !ok {
		t.Fatal("release checks job missing")
	}
	removed := false
	for i := 0; i < len(job.Permissions.Content); i += 2 {
		if job.Permissions.Content[i].Value == "issues" {
			job.Permissions.Content = append(job.Permissions.Content[:i], job.Permissions.Content[i+2:]...)
			removed = true
			break
		}
	}
	if !removed {
		t.Fatal("release checks issues permission missing")
	}
	release.Jobs["checks"] = job
	_, diagnostics := checkWorkflowPermissions(workflows)
	want := "release.yml/checks -> swarm-validate.yml/policy: issues requires read, available none"
	if len(diagnostics) != 1 || diagnostics[0] != want {
		t.Fatalf("got %v; want %s", diagnostics, want)
	}
	t.Log(diagnostics[0])
}

func TestWorkflowPermissionComparison(t *testing.T) {
	for _, tc := range []struct{ name, caller, callee, want string }{
		{"equal", "{contents: read}", "{contents: read}", ""},
		{"write covers read", "{contents: write}", "{contents: read}", ""},
		{"read below write", "{contents: read}", "{contents: write}", "contents requires write, available read"},
		{"missing scope", "{contents: read}", "{issues: read}", "issues requires read, available none"},
		{"none", "{issues: none}", "{issues: read}", "issues requires read, available none"},
		{"none request", "{}", "{issues: none}", ""},
		{"empty", "{}", "{contents: read}", "contents requires read, available none"},
		{"read all", "read-all", "{contents: read}", ""},
		{"write all", "write-all", "{id-token: write}", ""},
		{"read all excludes token", "read-all", "{id-token: write}", "id-token requires write, available none"},
		{"invalid level", "{contents: admin}", "{}", "invalid permission"},
		{"invalid token read", "{id-token: read}", "{}", "invalid permission"},
		{"invalid shape", "[]", "{}", "permissions must be"},
		{"invalid shorthand", "other", "{}", "unsupported permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var caller, callee permissionWorkflow
			if err := yaml.Unmarshal([]byte("jobs:\n  call:\n    uses: ./.github/workflows/callee.yaml\n    permissions: "+tc.caller), &caller); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte("jobs:\n  target:\n    permissions: "+tc.callee), &callee); err != nil {
				t.Fatal(err)
			}
			_, diagnostics := checkWorkflowPermissions(map[string]permissionWorkflow{"caller.yml": caller, "callee.yaml": callee})
			got := strings.Join(diagnostics, "\n")
			if (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
				t.Fatalf("diagnostics %q, want %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, caller, callee, want string
		calls                      int
	}{
		{"defaults", "permissions: {contents: read}\njobs:\n  a: {uses: ./.github/workflows/callee.yaml}", "permissions: {contents: read}\njobs:\n  a: {}", "", 1},
		{"job replaces defaults", "permissions: {issues: read}\njobs:\n  a: {uses: ./.github/workflows/callee.yaml, permissions: {}}", "permissions: {issues: read}\njobs:\n  a: {}", "issues requires read, available none", 1},
		{"callee replaces defaults", "permissions: {}\njobs:\n  a: {uses: ./.github/workflows/callee.yaml}", "permissions: {issues: write}\njobs:\n  a: {permissions: {}}", "", 1},
		{"later job and multiple calls", "permissions: {}\njobs:\n  a: {uses: ./.github/workflows/callee.yaml}\n  b: {uses: ./.github/workflows/callee.yaml}", "permissions: {}\njobs:\n  a: {}\n  z: {permissions: {issues: read}}", "callee.yaml/z: issues requires read", 2},
		{"unavailable defaults", "jobs:\n  a: {uses: ./.github/workflows/callee.yaml}", "jobs: {}", "effective permissions unavailable", 1},
		{"callee unavailable defaults", "permissions: {}\njobs:\n  a: {uses: ./.github/workflows/callee.yaml}", "jobs:\n  a: {}", "effective permissions unavailable", 1},
		{"missing target", "jobs:\n  a: {uses: ./.github/workflows/missing.yml}", "jobs: {}", "missing local workflow", 1},
		{"ignore remote and steps", "jobs:\n  a: {uses: owner/repo/.github/workflows/test.yml@main}\n  b:\n    steps:\n      - uses: ./.github/workflows/missing.yml", "jobs: {}", "", 0},
		{"malformed YAML", "jobs: [", "jobs: {}", "parse", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for file, raw := range map[string]string{"caller.yml": tc.caller, "callee.yaml": tc.callee, "ignored.txt": "["} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(strings.ReplaceAll(raw, "\n", "\r\n")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			workflows, err := loadPermissionWorkflows(dir)
			if tc.want == "parse" {
				if err == nil {
					t.Fatal("expected parse error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			calls, diagnostics := checkWorkflowPermissions(workflows)
			got := strings.Join(diagnostics, "\n")
			if calls != tc.calls || (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
				t.Fatalf("calls=%d diagnostics=%q; want calls=%d diagnostic=%q", calls, got, tc.calls, tc.want)
			}
		})
	}
}
