package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/progress"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkProgressPrintsCoordinatorOperation(t *testing.T) {
	for _, printer := range []func(*bytes.Buffer) func(progress.Event){func(b *bytes.Buffer) func(progress.Event) { return workProgressPrinter(b) }, func(b *bytes.Buffer) func(progress.Event) { return operationProgressPrinter(b) }} {
		for _, tc := range []struct {
			e    progress.Event
			want string
		}{
			{progress.Event{Role: "reviewer", Tool: "claude", ActivityKnown: true, ActivityAge: 12 * time.Second}, "; reviewer via claude, last activity 12s ago"},
			{progress.Event{Role: "reviewer", Tool: "mock"}, "; reviewer via mock, last activity unknown"},
			{progress.Event{Operation: "state read", OperationElapsed: 45 * time.Second}, "; coordinator state read for 45s"},
		} {
			var b bytes.Buffer
			tc.e.Status = "running"
			printer(&b)(tc.e)
			if !strings.Contains(b.String(), tc.want) {
				t.Fatal(b.String())
			}
			tc.e.Status = "completed"
			if activitySuffix(tc.e) != "" {
				t.Fatal("transition suffix")
			}
		}
		for _, bad := range []string{"reviewer --token=ghp_x", "../../etc", "\n\x1b[31m", "git push https://x:tok@h"} {
			var b bytes.Buffer
			printer(&b)(progress.Event{Status: "running", Role: bad, Tool: bad, Operation: bad})
			if strings.Contains(b.String(), bad) {
				t.Fatal(b.String())
			}
		}
	}
}

func TestReviewPlanProgressKeepsJSONOnStdout(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.must(t, "issue", "work", n, "--agent", "mock", "--phase", "plan")
	cmd := f.command(nil)
	var output, diag bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diag)
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "issue", "review-plan", n, "--agent", "mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %s", err, diag.String())
	}
	dec := json.NewDecoder(&output)
	var document map[string]any
	if err := dec.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document["status"] != "completed" {
		t.Fatal(document)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("extra stdout: %v", err)
	}
	if !strings.Contains(diag.String(), "review started") || !strings.Contains(diag.String(), "review completed") {
		t.Fatal(diag.String())
	}
}

type upstreamCLIReviewTracker struct{ tracker.Tracker }

func (t upstreamCLIReviewTracker) UpstreamRepo() string { return "octo/swarm" }
func (t upstreamCLIReviewTracker) VerifyPlanReviewFork(_ context.Context, slug, owner string) (string, error) {
	if slug != "alice/swarm" || owner != "alice" {
		return "", fmt.Errorf("invalid personal fork")
	}
	return slug, nil
}

type upstreamCLIReviewRunner struct {
	agent.MockRunner
	verdict string
}

func (r upstreamCLIReviewRunner) Provenance(string, bool) contract.RoleAttestation {
	return contract.RoleAttestation{Role: "reviewer", Agent: "codex", Harness: "codex", HarnessVersion: "fixture", Model: "review-fixture", Sandbox: "codex-native"}
}
func (r upstreamCLIReviewRunner) Run(_ context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	if !req.ReadOnly {
		return nil, fmt.Errorf("review must be read-only")
	}
	if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm/active/REVIEW.md"), []byte("VERDICT: "+r.verdict+"\n"), 0644); err != nil {
		return nil, err
	}
	return &agent.RunResult{Agent: "codex", Role: "reviewer", Attestation: r.Provenance("reviewer", true)}, nil
}
func TestUpstreamPlanReviewCLISeparateCheckout(t *testing.T) {
	for _, gate := range []string{"agent", "human"} {
		for _, verdict := range []string{"APPROVED", "CHANGES_REQUESTED"} {
			t.Run(gate+verdict, func(t *testing.T) {
				f := newUpstreamRemoteFixture(t)
				if gate == "human" {
					f.issues[42].Body = strings.Replace(f.issues[42].Body, "gate: agent", "gate: human", 1)
					if out, err := f.run(RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: f.maintainer}, "issue", "approve", "42"); err != nil {
						t.Fatalf("approve human task: %v %s", err, out)
					}
				}
				alice := f.aliceClone("review-contributor")
				out, err := f.run(f.as("alice", alice), "--upstream-remote", "upstream", "--fork", "issue", "work", "42", "--agent", "mock", "--phase", "plan", "--headless")
				if err != nil {
					t.Fatalf("planning: %v %s", err, out)
				}
				before := f.state()
				if before.HeadSHA != "" || before.Branch != "" || len(before.CheckpointHeads) != 0 || before.PRNumber != 0 || before.PRRevision != nil {
					t.Fatalf("not initial fork fixture: %+v", before)
				}
				opts := RootOptions{RepoRoot: f.upstreamRoot, Backend: "github", trackerOverride: upstreamCLIReviewTracker{f.maintainer}, lookupEnv: f.env.lookup(), nowFn: func() time.Time { return f.now }, resolverRunner: func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &upstreamCLIReviewRunner{verdict: verdict}, nil
				}}
				cmd := newRootCmdWithOptions(&opts)
				var output, diag bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&diag)
				cmd.SetArgs([]string{"issue", "review-plan", "42", "--agent", "codex", "--json"})
				err = cmd.ExecuteContext(context.Background())
				if verdict == "CHANGES_REQUESTED" {
					if !errors.Is(err, workflow.ErrPlanReviewRejected) {
						t.Fatalf("rejection: %v %s", err, diag.String())
					}
				} else if err != nil {
					t.Fatalf("standalone review: %v %s", err, diag.String())
				}
				decoder := json.NewDecoder(&output)
				var doc map[string]any
				if err := decoder.Decode(&doc); err != nil {
					t.Fatal(err)
				}
				if doc["kind"] != "review" || doc["verdict"] != verdict || doc["advisory"] != (gate == "human") || doc["reviewer_kind"] != "agent" {
					t.Fatal(doc)
				}
				var extra any
				if err := decoder.Decode(&extra); err != io.EOF {
					t.Fatalf("extra stdout: %v", err)
				}
				if !strings.Contains(diag.String(), "review started") || (verdict == "APPROVED" && !strings.Contains(diag.String(), "review completed")) {
					t.Fatal(diag.String())
				}
				after := f.state()
				if !reflect.DeepEqual(before.Lease, after.Lease) || !reflect.DeepEqual(before.ClaimBudget, after.ClaimBudget) {
					t.Fatal("CLI changed contributor lifecycle")
				}
			})
		}
	}
}
