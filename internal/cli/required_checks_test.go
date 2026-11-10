package cli

import (
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestConfiguredRequiredCheckEntryPoints verifies each entry point runs only the
// configured all-package test check using a scripted Go command.
func TestConfiguredRequiredCheckEntryPoints(t *testing.T) {
	f := newCLIFixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n"
	if err := testutil.WriteExecutable(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
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
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
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
	for _, step := range []struct {
		args []string
	}{
		{[]string{"issue", "work", n, "--agent", "mock", "--progress=false"}},
		{[]string{"issue", "validate", n, "--progress=false"}},
		{[]string{"issue", "submit", n, "--progress=false"}},
	} {
		if step.args[1] == "submit" {
			seedCompleteIssueAcceptance(t, f, mustIssueNumber(n))
		}
		f.must(t, step.args...)
		got := invocations()
		var tests []string
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			if strings.HasPrefix(line, "test ") {
				tests = append(tests, line)
			}
		}
		if len(tests) != 1 || tests[0] != "test -json ./..." {
			t.Fatalf("swarm %v ran go with:\n%s", step.args, got)
		}
	}
}
