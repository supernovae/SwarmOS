package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGoFastPassEntryPoints checks which commands run changed-package tests
// before a required `go test ./...` check. A scripted go command records each
// invocation, so no Go toolchain work happens here.
func TestGoFastPassEntryPoints(t *testing.T) {
	f := newCLIFixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	metadata := filepath.Join(bin, "metadata.json")
	module := `"Module":{"Path":"example.com/m","Dir":"/m","Main":true}`
	if err := os.WriteFile(metadata, []byte(`{"ImportPath":"example.com/m/leaf","Dir":"/m/leaf",`+module+"}\n"+`{"ImportPath":"example.com/m/other","Dir":"/m/other",`+module+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\nif [ \"$1\" = list ]; then cat '" + metadata + "'; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	policy := "required_checks:\n  - name: test\n    command: go test ./...\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
	for name, contents := range map[string]string{".swarm/validation/required-checks.yaml": policy, "go.mod": "module example.com/m\n\ngo 1.21\n"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "go policy"}} {
		if out, err := exec.Command("git", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	f.setenv("SWARM_MOCK_DEVELOPER_CMD", "mkdir -p leaf && printf 'package leaf\\n' > leaf/leaf.go")
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	invocations := func() string {
		t.Helper()
		data, _ := os.ReadFile(log)
		if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(data)
	}
	const preliminary = "test example.com/m/leaf\n"
	for _, step := range []struct {
		args []string
		fast bool
	}{
		{[]string{"issue", "work", n, "--agent", "mock", "--progress=false"}, true},
		{[]string{"issue", "validate", n, "--progress=false"}, true},
		{[]string{"issue", "submit", n, "--progress=false"}, false},
	} {
		f.must(t, step.args...)
		got := invocations()
		if strings.Contains(got, preliminary) != step.fast || !strings.HasSuffix(got, "test ./...\n") {
			t.Fatalf("swarm %v ran go with:\n%s", step.args, got)
		}
	}
}
