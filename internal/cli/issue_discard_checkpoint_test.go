package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestCLIDiscardCheckpointAndStatus(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{"branch", "hidden"} {
		t.Run(ns, func(t *testing.T) {
			t.Parallel()
			testCLIDiscardCheckpointAndStatus(t, ns)
		})
	}
}

// testCLIDiscardCheckpointAndStatus selects ns on the published default
// branch, the only configuration that chooses the ledger namespace, and gives
// the fixture manager the layout every github command resolves from it.
func testCLIDiscardCheckpointAndStatus(t *testing.T, ns string) {
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	ctx := context.Background()
	number := strconv.Itoa(n)
	remote := filepath.Join(t.TempDir(), "remote.git")
	prGit(t, f.root, "init", "--bare", "--initial-branch=main", remote)
	prGit(t, f.root, "remote", "add", "origin", remote)
	config := filepath.Join(f.root, ".swarm", "config.yaml")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	selected := regexp.MustCompile(`(?m)^ledger_namespace: .*$`).ReplaceAllString(string(data), "ledger_namespace: "+ns)
	if err = os.WriteFile(config, []byte(selected), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "commit", "--allow-empty", "-am", "select "+ns+" ledger refs")
	prGit(t, f.root, "push", "origin", "main")
	trusted, err := workspace.TrustedLedgerNamespace(f.root, "")
	if err != nil || trusted != ns {
		t.Fatalf("trusted namespace = %q, %v; want %s", trusted, err, ns)
	}
	layout, err := coordination.LayoutFor(trusted)
	if err != nil {
		t.Fatal(err)
	}
	f.manager.SetStore(f.root, coordination.NewGitStoreWithLayout(f.root, "", layout))
	if _, err := f.manager.ApproveTask(ctx, n, "alice"); err != nil {
		t.Fatal(err)
	}
	st, err := f.manager.Acquire(ctx, n, "alice", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	path, branch, err := workspace.ProvisionAt(f.root, n, "test", st.BaseSHA, st.Branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "abandoned.txt"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	prGit(t, path, "add", "abandoned.txt")
	prGit(t, path, "commit", "-m", "abandoned")
	c := prGit(t, path, "rev-parse", "HEAD")
	if _, err = f.manager.RecordCheckpoint(ctx, n, "alice", branch, c); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		opts := &RootOptions{RepoRoot: f.root, Repo: "test/repo", trackerOverride: checkpointCLITracker{f.tracker}}
		cmd := f.command(opts)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("swarm %v: %v %s", args, err, out)
		}
		return out
	}
	if out, err := run("issue", "discard-checkpoint", number, "--expected-head", c); err == nil || !strings.Contains(err.Error(), "swarm issue release "+number) {
		t.Fatalf("discard under an active claim: %v %s", err, out)
	}
	if _, err = f.manager.Release(ctx, n, "alice", "abandoned"); err != nil {
		t.Fatal(err)
	}
	prGit(t, f.root, "commit", "--allow-empty", "-m", "new base")
	prGit(t, f.root, "push", "origin", "main")
	if _, err = f.manager.ApproveTask(ctx, n, "alice"); err != nil {
		t.Fatal(err)
	}

	raw := must("issue", "discard-checkpoint", number, "--expected-head", c, "--json")
	var result struct {
		Kind      string                    `json:"kind"`
		Discarded lease.DiscardedCheckpoint `json:"discarded"`
	}
	ref := layout.CheckpointRef(n, c)
	if err = json.Unmarshal([]byte(raw), &result); err != nil || result.Kind != "checkpoint-discard" || result.Discarded.HeadSHA != c || result.Discarded.RetentionRef != ref || result.Discarded.DiscardedBy != "alice" {
		t.Fatalf("discard output: %s %v", raw, err)
	}
	if prGit(t, remote, "rev-parse", ref) != c {
		t.Fatal("abandoned checkpoint not retained upstream")
	}
	if out := prGit(t, remote, "for-each-ref", layout.Counterpart().CheckpointPrefix()); out != "" {
		t.Fatalf("retention landed in the unselected namespace:\n%s", out)
	}
	text := must("issue", "status", number)
	for _, want := range []string{"Discarded checkpoint: " + c, "retained at " + ref, "Fresh start: next claim begins on " + workspace.FreshStartBranch(n, c)} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
	raw = must("issue", "status", number, "--json")
	var status struct {
		Issues []struct {
			State *lease.IssueState `json:"state"`
		} `json:"issues"`
	}
	if err = json.Unmarshal([]byte(raw), &status); err != nil || len(status.Issues) != 1 {
		t.Fatalf("status json: %s %v", raw, err)
	}
	if got := status.Issues[0].State.DiscardedCheckpoints; len(got) != 1 || got[0].HeadSHA != c || got[0].RetentionRef != ref || got[0].Branch != branch {
		t.Fatalf("status json discard history: %+v", got)
	}
	// A repeated discard finds no recorded checkpoint and changes nothing.
	if _, err := run("issue", "discard-checkpoint", number, "--expected-head", c); err == nil || !strings.Contains(err.Error(), "no recorded checkpoint") {
		t.Fatalf("repeat discard: %v", err)
	}
}

func TestDiscardCheckpointFlags(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	help := f.must(t, "issue", "discard-checkpoint", "--help")
	for _, want := range []string{"--expected-head", "--json", "maintainer", "refs/heads/swarm-checkpoints/", "refs/swarm/checkpoints/"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %s", want)
		}
	}
	for _, args := range [][]string{
		{"issue", "discard-checkpoint", "1"},
		{"issue", "discard-checkpoint", "x", "--expected-head", strings.Repeat("a", 40)},
		{"issue", "discard-checkpoint", "--expected-head", strings.Repeat("a", 40)},
	} {
		if _, err := f.run(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := f.run("issue", "discard-checkpoint", "1"); err == nil || !strings.Contains(err.Error(), "--expected-head is required") {
		t.Fatalf("missing expected head: %v", err)
	}
}

func TestCLIArchiveCompletion(t *testing.T) {
	for _, scenario := range []string{"released", "expired", "recorded", "recorded-json"} {
		t.Run(scenario, func(t *testing.T) {
			asJSON := scenario == "recorded-json"
			recorded := scenario == "recorded" || asJSON
			f := newCLIFixture(t)
			n := f.create(t, "agent")
			ctx := t.Context()
			number := strconv.Itoa(n)
			f.must(t, "issue", "approve", number)
			f.must(t, "issue", "work", number, "--phase", "plan", "--agent", "mock", "--progress=false")
			f.must(t, "issue", "review-plan", number, "--agent", "mock", "--progress=false")
			st := f.state(t, n)
			f.manager.BindClaim(n, st.Lease.ClaimID)
			path := workspace.WorktreePath(f.root, n)
			prGit(t, path, "commit", "--allow-empty", "-m", "candidate")
			candidate := prGit(t, path, "rev-parse", "HEAD")
			if err := os.MkdirAll(filepath.Join(path, ".swarm/active"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, ".swarm/active/IMPLEMENTATION.md"), []byte("Implemented"), 0600); err != nil {
				t.Fatal(err)
			}
			md := lease.ReviewBindings(st)
			md["head_sha"] = candidate
			art := contract.Artifact{Type: contract.ArtifactImplementation, Verdict: "PASS", Body: "Implemented", Metadata: md}
			if err := f.manager.SaveCheckpointCompletion(ctx, n, "alice", st.BaseSHA, candidate, art); err != nil {
				t.Fatal(err)
			}
			if recorded {
				if _, err := f.manager.RecordCheckpoint(ctx, n, "alice", st.Branch, candidate); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "expired" {
				rewriteRecoveryState(t, f, n, func(st *lease.IssueState) {
					st.Lease.ExpiresAt = time.Now().Add(-time.Hour)
				})
				// Claim can record a successor before reporting checkout divergence.
				// Preserve the original candidate in either case.
				_, claimErr := f.run("issue", "claim", number, "--agent", "mock")
				if current := f.state(t, n); current.Lease.ClaimID == st.Lease.ClaimID || prGit(t, path, "rev-parse", "HEAD") != candidate {
					t.Fatalf("claim did not establish a safe successor: %v", claimErr)
				}
			} else {
				f.must(t, "issue", "release", number)
				rewriteRecoveryState(t, f, n, func(st *lease.IssueState) {
					st.ApprovedBy = "another-maintainer"
				})
			}
			doc, err := f.runJSON(t, "issue", "work", number, "--agent", "mock", "--progress=false", "--json")
			var changed *lease.CompletionInputsChangedError
			if !errors.As(err, &changed) {
				t.Fatalf("expected changed inputs from work: %v\n%v", err, doc)
			}
			args := []string{"issue", "archive-completion", number, "--expected-receipt", changed.ReceiptID}
			assertNextAction(t, doc, err, ReasonCompletionInputsChanged, true, ExitFailure, append([]string{"swarm"}, args...)...)
			ref := "refs/swarm/local-completions/issue-" + number + "/" + candidate
			if asJSON {
				out, err := f.runJSON(t, append(args, "--json")...)
				if err != nil || out["kind"] != "completion-archive" ||
					out["candidate"] != candidate || out["retention_ref"] != ref || out["already_recorded"] != true {
					t.Fatalf("structured archive: %v, %v", out, err)
				}
			} else {
				out := f.must(t, args...)
				if !strings.Contains(out, ref) || strings.Contains(out, "already recorded") != recorded || strings.Contains(out, "--dry-run") == recorded {
					t.Fatalf("recovery guidance: %s", out)
				}
			}
			if prGit(t, f.root, "rev-parse", ref) != candidate {
				t.Fatalf("candidate not retained at %s", ref)
			}
			if _, err := f.run(args...); !errors.Is(err, lease.ErrClaimFence) || !strings.Contains(err.Error(), "no matching pending completion receipt") {
				t.Fatalf("repeat archive: %v", err)
			}
		})
	}
	f := newCLIFixture(t)
	out, err := f.runJSON(t, "issue", "archive-completion", "1", "--json")
	if err == nil || out["kind"] != "completion-archive" || out["status"] != "failed" ||
		out["error"] != "--expected-receipt is required" {
		t.Fatalf("structured refusal: %v, %v", out, err)
	}
	help := f.must(t, "issue", "archive-completion", "--help")
	for _, want := range []string{"--expected-receipt", "maintainer", "original coordinator checkout"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %q: %s", want, help)
		}
	}
	if _, err := f.run("--fork", "issue", "archive-completion", "1", "--expected-receipt", strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "direct coordinator checkout") {
		t.Fatalf("fork archive: %v", err)
	}
}
