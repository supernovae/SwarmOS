package workflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/tracker"
)

func setupProjectPlanTestRepo(t *testing.T) (string, *Engine, *tracker.LocalTracker) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("SWARM_USER", "maintainer")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Plan Test")
	git("config", "user.email", "test@example.invalid")
	if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "api", "handler.go"), []byte("package api\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial commit")

	tr := tracker.NewLocalTracker(root)
	eng, err := NewEngine(root, tr)
	if err != nil {
		t.Fatal(err)
	}
	return root, eng, tr
}

func TestRunProjectPlanFromMarkdownRoadmapAndScanBacklog(t *testing.T) {
	root, eng, tr := setupProjectPlanTestRepo(t)
	ctx := context.Background()

	roadmapPath := filepath.Join(root, "ROADMAP.md")
	roadmap := `# Q4 Roadmap

## Add rate limiter middleware in internal/api/handler.go
Implement token-bucket rate limiting for API endpoints in internal/api/handler.go.
- [ ] Rate limiter middleware blocks burst traffic
- [ ] Unit tests cover rate limit headers

## Add audit logging to internal/api/handler.go
Emit structured audit events from internal/api/handler.go after each request.
- [ ] Audit log entries include status code and latency
`
	if err := os.WriteFile(roadmapPath, []byte(roadmap), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Decompose roadmap and apply + approve
	fromReport, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot: root,
		FromFile: roadmapPath,
		Apply:    true,
		Ready:    true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --from: %v", err)
	}
	if len(fromReport.CreatedIssues) != 2 {
		t.Fatalf("expected 2 created issues, got %d", len(fromReport.CreatedIssues))
	}
	if !slices.Contains(fromReport.CreatedIssues[0].Labels, "swarm:ready") {
		t.Fatalf("expected issue #1 to have swarm:ready, got %v", fromReport.CreatedIssues[0].Labels)
	}
	if len(fromReport.ProposedUnits[1].DependsOn) != 1 || fromReport.ProposedUnits[1].DependsOn[0] != fromReport.CreatedIssues[0].Number {
		t.Fatalf("expected issue #2 to depend on #%d, got %v", fromReport.CreatedIssues[0].Number, fromReport.ProposedUnits[1].DependsOn)
	}

	// 2. Create one unstructured raw issue and one issue touching a protected path
	rawIss, err := tr.CreateIssue(ctx, "Fix panic on empty request body", "When an empty POST body hits internal/api/handler.go it panics.\n- Add nil check and test", nil)
	if err != nil {
		t.Fatal(err)
	}
	riskyIss, err := tr.CreateIssue(ctx, "Modify CI workflow", "### Objective\n\nUpdate .github/workflows/swarm-validate.yml\n\n### Constraints\n\n- Keep fast\n\n### Acceptance Criteria\n\n- [ ] Passes\n\n### Workflow\n\nfeature\n\n### Plan Gate\n\nagent\n", nil)
	if err != nil {
		t.Fatal(err)
	}

	// First scan without normalize to verify swarm:needs-scope and swarm:blocked detection
	scanPreview, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot: root,
		ScanOpen: true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --scan preview: %v", err)
	}
	if len(scanPreview.BacklogItems) != 4 {
		t.Fatalf("expected 4 backlog items, got %d", len(scanPreview.BacklogItems))
	}
	var foundRaw, foundRisky bool
	for _, it := range scanPreview.BacklogItems {
		if it.IssueNumber == rawIss.Number {
			foundRaw = true
			if it.ValidWorkUnit || it.RecommendedLabel != "swarm:needs-scope" {
				t.Fatalf("expected raw issue to be invalid with swarm:needs-scope, got %+v", it)
			}
			if !slices.Contains(it.OverlapsWith, 1) {
				t.Fatalf("expected raw issue touching internal/api/handler.go to overlap with #1, got %v", it.OverlapsWith)
			}
		}
		if it.IssueNumber == riskyIss.Number {
			foundRisky = true
			if it.GoalAlignment != "RISK_FLAGGED" || it.RecommendedLabel != "swarm:blocked" {
				t.Fatalf("expected protected-path issue to be RISK_FLAGGED / swarm:blocked, got %+v", it)
			}
		}
	}
	if !foundRaw || !foundRisky {
		t.Fatalf("missing expected backlog items: raw=%t risky=%t", foundRaw, foundRisky)
	}

	// Now apply with --normalize and verify raw issue is converted to a valid WorkUnit with swarm:draft
	scanApplied, err := eng.RunProjectPlan(ctx, ProjectPlanOptions{
		RepoRoot:  root,
		ScanOpen:  true,
		Apply:     true,
		Normalize: true,
	})
	if err != nil {
		t.Fatalf("RunProjectPlan --scan --apply --normalize: %v", err)
	}
	updatedRaw, err := tr.GetIssue(ctx, rawIss.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updatedRaw.Body, "### Objective") || !slices.Contains(updatedRaw.Labels, "swarm:draft") {
		t.Fatalf("expected normalized raw issue to have WorkUnit sections and swarm:draft, got labels=%v body=%s", updatedRaw.Labels, updatedRaw.Body)
	}
	if len(scanApplied.ExecutionWaves) < 2 {
		t.Fatalf("expected overlapping issues to be separated into at least 2 execution waves, got %v", scanApplied.ExecutionWaves)
	}
}
