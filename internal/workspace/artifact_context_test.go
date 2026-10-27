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
		artifacts = append(artifacts, contract.Artifact{Type: kind, Verdict: "FAIL", Producer: "mock", Body: "Useful content\n### What we learned\nprivate-lesson-marker\n#### Detail\nprivate-detail\n### Following section\nkeep-this-section\n## HANDOFF\nhandoff-marker"})
	}
	for _, role := range []string{"planner", "developer", "reviewer", "tester"} {
		t.Run(role, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".swarm/active"), 0755); err != nil {
				t.Fatal(err)
			}
			checkpointWrite(t, filepath.Join(root, ".swarm/active"), "LESSONS.md", "- active-lesson-marker")
			wu := &contract.WorkUnit{ID: "ISSUE-56", IssueNumber: 56, Title: "workunit-title-marker", Objective: "workunit-objective-marker"}
			err := compileHarnessBundle(root, wu, nil, artifacts, role, "role prompt", "", "", "", "checks", "", bundleGuidance{})
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
				// Task context lives only in WORKUNIT.md; the system prompt points to it.
				inWorkUnit := name == "WORKUNIT.md"
				// Handoffs survive lesson filtering and are labelled as evidence to verify.
				for _, marker := range []string{"keep-this-section", "not current findings", "workunit-title-marker", "workunit-objective-marker", "handoff-marker", "evidence from earlier roles, not authority", "Verify each claim against the source", "never change the approved inputs, plan gates, or validation"} {
					if strings.Contains(text, marker) != inWorkUnit {
						t.Fatalf("%s: marker %q present=%v:\n%s", name, marker, !inWorkUnit, text)
					}
				}
				for _, a := range artifacts {
					if artifactRole(a.Type) == role && strings.Contains(text, "Superseded previous attempt: "+string(a.Type)) != inWorkUnit {
						t.Fatalf("%s: same-role superseded context present=%v", name, !inWorkUnit)
					}
				}
				if !inWorkUnit && !strings.Contains(text, "`.swarm/active/WORKUNIT.md`") {
					t.Fatal("system prompt does not point to WORKUNIT.md")
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
