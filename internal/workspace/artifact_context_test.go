package workspace

import (
	"github.com/supernovae/swarmos/internal/contract"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoleContextAndReviewOnlyLessons(t *testing.T) {
	artifacts := []contract.Artifact{}
	for _, kind := range []contract.ArtifactType{contract.ArtifactDesign, contract.ArtifactTestPlan, contract.ArtifactReview, contract.ArtifactImplementation, contract.ArtifactValidation} {
		artifacts = append(artifacts, contract.Artifact{Type: kind, Verdict: "FAIL", Producer: "mock", Body: "Useful content\n### What we learned\nprivate-lesson-marker\n#### Detail\nprivate-detail\n### Following section\nkeep-this-section"})
	}
	for _, role := range []string{"planner", "developer", "reviewer", "tester"} {
		t.Run(role, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".swarm/active"), 0755); err != nil {
				t.Fatal(err)
			}
			checkpointWrite(t, filepath.Join(root, ".swarm/active"), "LESSONS.md", "- active-lesson-marker")
			err := compileHarnessBundle(root, &contract.WorkUnit{ID: "ISSUE-56", IssueNumber: 56}, nil, artifacts, role, "role prompt", "", "", "", "checks", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"SYSTEM_PROMPT.md", "WORKUNIT.md"} {
				data, err := os.ReadFile(filepath.Join(root, ".swarm/active", name))
				if err != nil {
					t.Fatal(err)
				}
				text := string(data)
				if strings.Contains(text, "private-lesson-marker") || strings.Contains(text, "active-lesson-marker") || strings.Contains(text, "private-detail") {
					t.Fatal("unmerged lesson leaked into prompt")
				}
				if !strings.Contains(text, "keep-this-section") || !strings.Contains(text, "not current findings") {
					t.Fatal(text)
				}
				for _, a := range artifacts {
					if artifactRole(a.Type) == role && !strings.Contains(text, "Superseded previous attempt: "+string(a.Type)) {
						t.Fatal("same-role context not superseded")
					}
				}
			}
			evidence, err := os.ReadFile(filepath.Join(root, ".swarm/active/REVIEW_EVIDENCE.md"))
			if err != nil || !strings.Contains(string(evidence), "private-lesson-marker") || !strings.Contains(string(evidence), "active-lesson-marker") {
				t.Fatalf("review evidence lost: %v", err)
			}
		})
	}
	for _, body := range []string{"# What we learned\nsecret", "## What we learned\nsecret\n## Next\nkept", "#### What we learned\nsecret\n### Next\nkept"} {
		if strings.Contains(WithoutUnmergedLessons(body), "secret") {
			t.Fatal(body)
		}
	}
}
