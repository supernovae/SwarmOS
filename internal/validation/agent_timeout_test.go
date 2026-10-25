package validation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestRequiredChecksIgnoreAgentTimeoutPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX sleep")
	}
	for _, test := range []struct {
		agent, check string
		pass         bool
	}{{"1ns", "1m", true}, {"24h", "1ns", false}} {
		t.Run(test.agent, func(t *testing.T) {
			repo := setupGitRepoForValidation(t)
			config := filepath.Join(repo, ".swarm", "config.yaml")
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "execution:", "execution:\n  agent_timeout: "+test.agent, 1))
			if err := os.WriteFile(config, data, 0600); err != nil {
				t.Fatal(err)
			}
			policy := "required_checks:\n  - name: timed\n    command: sleep 0.05\n    timeout: " + test.check + "\nprotected_paths: ['.github/workflows/**']\n"
			if err := os.WriteFile(filepath.Join(repo, ".swarm", "validation", "required-checks.yaml"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "timeout policy"}} {
				cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s %v", out, err)
				}
			}
			base := gitRevision(t, repo, "HEAD")
			report, err := Validate(context.Background(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base})
			if test.pass && (err != nil || report == nil || !report.Passed) {
				t.Fatalf("agent policy shortened required check: %+v %v", report, err)
			}
			if !test.pass && (err == nil || report == nil) {
				t.Fatal("agent policy extended required check")
			}
			if _, err := os.Stat(filepath.Join(repo, ".swarm", "active-output")); !os.IsNotExist(err) {
				t.Fatalf("validation exported agent recovery: %v", err)
			}
		})
	}
}
