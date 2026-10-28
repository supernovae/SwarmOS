package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
)

func TestIssueDimensions(t *testing.T) {
	for _, backend := range []string{"local", "github"} {
		t.Run(backend, func(t *testing.T) {
			f := newCLIFixture(t)
			var tr tracker.Tracker = f.tracker
			if backend == "github" {
				addTestOrigin(t, f)
				_, tr = newFakeGitHubTracker(t)
			}
			run := func(args ...string) string {
				t.Helper()
				out, err := runIssueCmd(f, tr, args...)
				if err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
				return out
			}
			out := run("create", "--title", "Dimensions", "--objective", "Implement dimension support", "--constraint", "Stay scoped", "--acceptance", "Tests pass", "--size", "small", "--uncertainty", "known", "--risk", "sensitive", "--json")
			var result struct {
				Issue tracker.Issue `json:"issue"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err, out)
			}
			n := strconv.Itoa(result.Issue.Number)
			var lint struct {
				Dimensions map[string]string `json:"dimensions"`
				Suggested  map[string]string `json:"suggested_dimensions"`
			}
			if err := json.Unmarshal([]byte(run("lint", n, "--json")), &lint); err != nil || lint.Dimensions["risk"] != "sensitive" || lint.Suggested["size"] == "" {
				t.Fatalf("lint: %+v %v", lint, err)
			}
			run("approve", n)
			out = run("edit", n, "--risk=urgent")
			if !strings.Contains(out, "revision requires separate approval") {
				t.Fatal(out)
			}
			issue, err := tr.GetIssue(context.Background(), result.Issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			wu, err := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
			if err != nil || wu.Size != "small" || wu.Risk != "urgent" {
				t.Fatalf("stored: %+v %v", wu, err)
			}
			for _, command := range []string{"claim", "work"} {
				_, err := runIssueCmd(f, tr, command, n)
				var approval *lease.TaskApprovalError
				if !errors.As(err, &approval) {
					t.Fatalf("%s should refuse stale approval, got %v", command, err)
				}
			}
			run("approve", n)
			run("edit", n, "--size=", "--uncertainty=")
		})
	}
}
