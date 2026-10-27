package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestStatusPolicyChangeKeepsHumanPlan covers `issue status` and `issue work`
// after a human-gated issue is re-approved on a base whose policy changed.
func TestStatusPolicyChangeKeepsHumanPlan(t *testing.T) {
	t.Parallel()
	approved := func(t *testing.T, changed string) (*cliFixture, string) {
		f := newCLIFixture(t)
		n := strconv.Itoa(f.create(t, "human"))
		f.must(t, "issue", "approve", n)
		f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
		f.setenv("SWARM_USER", "independent-maintainer")
		f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Checked exact plan and test plan")
		f.setenv("SWARM_USER", "alice")
		f.must(t, "issue", "release", n)
		if err := os.WriteFile(filepath.Join(f.root, changed), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prGit(t, f.root, "add", changed)
		prGit(t, f.root, "commit", "-m", "base change")
		f.must(t, "issue", "approve", n)
		return f, n
	}

	t.Run("unchanged policy", func(t *testing.T) {
		t.Parallel()
		f, n := approved(t, "source.txt")
		if text := f.must(t, "issue", "status", n); strings.Contains(text, "Plan review required") || strings.Contains(text, "Plan: ") {
			t.Fatalf("unchanged policy printed a review hint:\n%s", text)
		}
	})

	t.Run("policy changed", func(t *testing.T) {
		t.Parallel()
		f, n := approved(t, filepath.Join(".swarm", "rules", "new.md"))
		text := f.must(t, "issue", "status", n)
		for _, want := range []string{"Plan: policy files changed\n", "Policy paths: .swarm/rules/new.md\n", "Plan review required: swarm issue review-plan " + n + " --approve\n"} {
			if !strings.Contains(text, want) {
				t.Fatalf("status missing %q:\n%s", want, text)
			}
		}
		out, err := f.run("issue", "work", n, "--agent", "mock", "--headless")
		if err != nil || strings.Contains(out, "missing TEST_PLAN.md") {
			t.Fatalf("work on kept plan: %v\n%s", err, out)
		}
		f.setenv("SWARM_USER", "independent-maintainer")
		f.must(t, "issue", "review-plan", n, "--approve", "--notes", "Re-checked under the new policy")
		f.setenv("SWARM_USER", "alice")
		if text := f.must(t, "issue", "status", n); strings.Contains(text, "Plan review required") {
			t.Fatalf("hint survived a fresh review:\n%s", text)
		}
		f.must(t, "issue", "work", n, "--agent", "mock", "--headless")
		f.must(t, "issue", "submit", n)
	})
}
