package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

func TestIssueCreateConstraints(t *testing.T) {
	for _, tc := range []struct {
		name        string
		constraints []string
		ready       bool
	}{{"omitted", []string{}, false}, {"ready", []string{}, true}, {"single", []string{"one"}, false}, {"repeated", []string{"one", "two"}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			run := func(args ...string) string {
				t.Helper()
				out, err := runIssueCmd(f, f.tracker, args...)
				if err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
				return out
			}
			args := []string{"create", "--title", "Constraints", "--objective", "Exercise empty constraints", "--acceptance", "Tests pass", "--json"}
			for _, c := range tc.constraints {
				args = append(args, "--constraint", c)
			}
			if tc.ready {
				args = append(args, "--ready")
			}
			var result struct {
				Issue tracker.Issue `json:"issue"`
			}
			if err := json.Unmarshal([]byte(run(args...)), &result); err != nil {
				t.Fatal(err)
			}
			n := strconv.Itoa(result.Issue.Number)
			check := func() {
				t.Helper()
				iss, err := f.tracker.GetIssue(context.Background(), result.Issue.Number)
				if err != nil {
					t.Fatal(err)
				}
				wu, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(wu.Constraints, tc.constraints) {
					t.Fatalf("constraints: %#v", wu.Constraints)
				}
				if err := contract.ValidateWorkUnit(wu); err != nil {
					t.Fatal(err)
				}
				if len(tc.constraints) == 0 {
					section := strings.Split(strings.Split(iss.Body, "### Constraints\n")[1], "### Acceptance Criteria")[0]
					if strings.TrimSpace(section) != "" {
						t.Fatal(section)
					}
				}
				if !strings.Contains(iss.Body, "- [ ] Tests pass") {
					t.Fatal(iss.Body)
				}
			}
			check()
			var lint struct {
				WorkUnit contract.WorkUnit `json:"work_unit"`
			}
			out := run("lint", n, "--json")
			if err := json.Unmarshal([]byte(out), &lint); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lint.WorkUnit.Constraints, tc.constraints) {
				t.Fatalf("lint: %s", out)
			}
			if len(tc.constraints) == 0 && !strings.Contains(out, "\"constraints\":[]") {
				t.Fatal(out)
			}
			if out := run("lint", n); !strings.Contains(out, fmt.Sprintf("constraints=%d", len(tc.constraints))) {
				t.Fatal(out)
			}
			if !tc.ready {
				run("approve", n)
			}
			state := f.state(t, result.Issue.Number)
			if state.TaskDigest == "" || state.PolicyDigest == "" || state.BaseSHA == "" {
				t.Fatalf("unbound approval: %+v", state)
			}
			check()
		})
	}
	for _, tc := range []struct {
		name  string
		extra []string
	}{{"empty constraint", []string{"--constraint=", "--acceptance", "Tests pass"}}, {"missing acceptance", nil}, {"empty acceptance", []string{"--acceptance="}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIFixture(t)
			args := append([]string{"create", "--title", "Invalid", "--objective", "Reject invalid arrays"}, tc.extra...)
			if _, err := runIssueCmd(f, f.tracker, args...); err == nil {
				t.Fatal("invalid input accepted")
			}
			issues, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
			if err != nil || len(issues) != 0 {
				t.Fatalf("created issues: %v %v", issues, err)
			}
		})
	}
}
