package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestFreshClaimBase(t *testing.T) {
	for _, tc := range []string{"none", "agent", "human", "unchanged", "policy", "task", "old digest", "missing tree", "nonancestor", "checkpoint", "branch", "fresh start", "partial none", "partial agent", "test only", "review only", "plan and test", "task unchanged", "old digest unchanged", "reviewed", "reviewed none"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			m, lt, issue := testManager(t)
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = m.repoRoot
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			gate := "agent"
			if tc == "none" || tc == "partial none" || tc == "reviewed none" {
				gate = "none"
			}
			if tc == "human" || tc == "unchanged" {
				gate = "human"
			}
			issue.Body = strings.Replace(issue.Body, "gate: human", "gate: "+gate, 1)
			if _, err := lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
				t.Fatal(err)
			}
			initial, err := m.ApproveTask(ctx, issue.Number, "local-dev")
			if err != nil {
				t.Fatal(err)
			}
			if tc == "reviewed" || tc == "reviewed none" {
				if _, err = m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1); err != nil {
					t.Fatal(err)
				}
				for _, a := range []contract.Artifact{{Type: contract.ArtifactDesign, Producer: "mock", Summary: "plan", Body: "plan"}, {Type: contract.ArtifactTestPlan, Producer: "mock", Summary: "tests", Body: "tests"}} {
					if _, err = m.UpdateArtifact(ctx, issue.Number, "local-dev", a); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = m.RecordHumanReview(ctx, issue.Number, "APPROVED", "review"); err != nil {
					t.Fatal(err)
				}
				if _, err = m.Release(ctx, issue.Number, "local-dev", "test"); err != nil {
					t.Fatal(err)
				}
			}
			st, rev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "checkpoint":
				st.HeadSHA = initial.BaseSHA
			case "branch":
				st.Branch = "existing"
			case "fresh start":
				st.Branch = workspace.FreshStartBranch(issue.Number, initial.BaseSHA)
				st.FreshStart = &workspace.FreshStart{Branch: st.Branch, AbandonedBranch: "swarm/issue-1-old", AbandonedHeadSHA: initial.BaseSHA}
			case "missing tree":
				st.PolicyTreeDigest = ""
			case "old digest", "old digest unchanged":
				st.PolicyDigest = "stale"
			case "partial none", "partial agent":
				st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: "partial"}}
			case "test only":
				st.Artifacts = []contract.Artifact{{Type: contract.ArtifactTestPlan, Body: "partial"}}
			case "review only":
				st.Artifacts = []contract.Artifact{{Type: contract.ArtifactReview, Body: "VERDICT: APPROVED"}}
			case "plan and test":
				st.Artifacts = []contract.Artifact{{Type: contract.ArtifactDesign, Body: "partial"}, {Type: contract.ArtifactTestPlan, Body: "partial"}}
			}
			if _, err = m.save(ctx, issue.Number, rev, st); err != nil {
				t.Fatal(err)
			}
			if tc == "task" || tc == "task unchanged" {
				issue.Body = strings.Replace(issue.Body, "Harden coordination state", "Changed objective", 1)
				if _, err = lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
					t.Fatal(err)
				}
			}
			if tc == "nonancestor" {
				git("checkout", "--orphan", "other")
			}
			name := "source.txt"
			if tc == "policy" {
				name = ".swarm/new.md"
			}
			if err = os.WriteFile(filepath.Join(m.repoRoot, name), []byte("change"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc != "unchanged" && tc != "task unchanged" && tc != "old digest unchanged" {
				git("add", name)
				git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "advance")
			}
			_, beforeRev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
			refused := tc == "human" || tc == "policy" || tc == "task" || tc == "old digest" || tc == "missing tree" || tc == "nonancestor" || strings.HasPrefix(tc, "partial") || tc == "test only" || tc == "review only" || tc == "plan and test" || tc == "task unchanged" || tc == "old digest unchanged"
			if refused {
				var typed *ClaimBaseApprovalError
				if !errors.As(err, &typed) || !errors.Is(err, ErrApprovalRequired) {
					t.Fatalf("expected typed refusal, got %v", err)
				}
				after, r, e := m.load(ctx, issue.Number)
				if e != nil {
					t.Fatal(e)
				}
				if r != beforeRev || after.BaseSHA != initial.BaseSHA || after.Lease.Status == "active" {
					t.Fatal("refusal mutated state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := git("rev-parse", "HEAD")
			if tc == "checkpoint" || tc == "fresh start" {
				want = initial.BaseSHA
			}
			if got.BaseSHA != want {
				t.Fatalf("base %s want %s", got.BaseSHA, want)
			}
			// A reserved branch without a checkpoint stays untouched and keeps its name.
			wantBranch := workspace.BranchName(issue.Number, issue.Title)
			switch tc {
			case "branch":
				wantBranch = "existing"
			case "fresh start":
				wantBranch = workspace.FreshStartBranch(issue.Number, initial.BaseSHA)
			}
			if got.Branch != wantBranch {
				t.Fatalf("branch %q want %q", got.Branch, wantBranch)
			}
			if got.ApprovedBy != initial.ApprovedBy || !got.ApprovedAt.Equal(initial.ApprovedAt) {
				t.Fatal("approval identity changed")
			}
			if strings.HasPrefix(tc, "reviewed") {
				wu, e := contract.ParseWorkUnitFromIssue(issue.Number, issue.Title, issue.Body)
				if e != nil {
					t.Fatal(e)
				}
				if got.ApprovalTransition == nil || VerifyApproval(got, wu) != nil {
					t.Fatal("review did not carry")
				}
			} else if got.ApprovalTransition != nil {
				t.Fatal("empty evidence created transition")
			}
		})
	}
}

type claimRaceStore struct {
	coordination.Store
	attempts int
}

func (s *claimRaceStore) CompareAndSwap(ctx context.Context, n int, rev string, data []byte) (string, error) {
	s.attempts++
	body, current, err := s.Store.Load(ctx, n)
	if err != nil {
		return "", err
	}
	var winner IssueState
	if err = json.Unmarshal(body, &winner); err != nil {
		return "", err
	}
	winner.CarryReason = "concurrent winner"
	body, err = json.Marshal(winner)
	if err != nil {
		return "", err
	}
	if _, err = s.Store.CompareAndSwap(ctx, n, current, body); err != nil {
		return "", err
	}
	return s.Store.CompareAndSwap(ctx, n, rev, data)
}

func TestFreshClaimBaseCASConflict(t *testing.T) {
	ctx := context.Background()
	m, lt, issue := testManager(t)
	issue.Body = strings.Replace(issue.Body, "gate: human", "gate: agent", 1)
	if _, err := lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	initial, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "advance")
	cmd.Dir = m.repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	racing := &claimRaceStore{Store: m.store}
	m.store = racing
	_, err = m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if !errors.Is(err, coordination.ErrConflict) || !errors.Is(err, ErrClaimRaceLost) {
		t.Fatalf("lost conflict identity: %v", err)
	}
	got, _, err := m.load(ctx, issue.Number)
	if err != nil {
		t.Fatal(err)
	}
	if racing.attempts != 1 || got.CarryReason != "concurrent winner" || got.BaseSHA != initial.BaseSHA || got.Lease.Status == "active" {
		t.Fatal("overwrote winner or retried")
	}
}

func TestFreshClaimBaseRemoteDefault(t *testing.T) {
	ctx := context.Background()
	m, lt, issue := testManager(t)
	issue.Body = strings.Replace(issue.Body, "gate: human", "gate: agent", 1)
	if _, err := lt.UpdateIssueBody(ctx, issue.Number, issue.Body); err != nil {
		t.Fatal(err)
	}
	initial, err := m.ApproveTask(ctx, issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	git := func(root string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	git(m.repoRoot, "clone", "--bare", m.repoRoot, remote)
	git(m.repoRoot, "remote", "add", "origin", remote)
	writer := filepath.Join(t.TempDir(), "writer")
	git(m.repoRoot, "clone", remote, writer)
	git(writer, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "advance remote")
	tip := git(writer, "rev-parse", "HEAD")
	git(writer, "push", "origin", "HEAD")
	if git(m.repoRoot, "rev-parse", "HEAD") != initial.BaseSHA {
		t.Fatal("worker should remain stale")
	}
	got, err := m.Acquire(ctx, issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSHA != tip || got.ClaimPreviousBase != initial.BaseSHA {
		t.Fatal("did not fetch advertised default tip")
	}
}

func TestFreshClaimBaseEarlyErrors(t *testing.T) {
	for _, tc := range []string{"load", "config", "allowlist", "trust", "authentication", "active", "resumed stale"} {
		t.Run(tc, func(t *testing.T) {
			ctx := context.Background()
			m, lt, issue := testManager(t)
			st, rev, err := m.load(ctx, issue.Number)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "load":
				if err = os.Remove(filepath.Join(m.repoRoot, ".swarm", "coordination", fmt.Sprintf("issue-%d.json", issue.Number))); err != nil {
					t.Fatal(err)
				}
			case "config":
				st.BaseSHA = strings.Repeat("0", 40)
			case "allowlist":
				path := filepath.Join(m.repoRoot, ".swarm", "config.yaml")
				body, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				body = []byte(strings.Replace(string(body), "security:\n", "security:\n  approved_collaborators: [other]\n", 1))
				if e = os.WriteFile(path, body, 0600); e != nil {
					t.Fatal(e)
				}
				cmd := exec.Command("git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-am", "restrict collaborators")
				cmd.Dir = m.repoRoot
				if out, e := cmd.CombinedOutput(); e != nil {
					t.Fatalf("%v %s", e, out)
				}
				if _, e = m.ApproveTask(ctx, issue.Number, "local-dev"); e != nil {
					t.Fatal(e)
				}
			case "trust":
				if err = lt.RemoveLabel(ctx, issue.Number, "swarm:ready"); err != nil {
					t.Fatal(err)
				}
			case "active":
				st.Lease.Status = "active"
				st.Lease.ExpiresAt = time.Now().Add(time.Hour)
			case "resumed stale":
				st.Branch = "existing"
				st.HeadSHA = st.BaseSHA
				st.TaskDigest = "stale"
			}
			if tc == "config" || tc == "active" || tc == "resumed stale" {
				if _, err = m.save(ctx, issue.Number, rev, st); err != nil {
					t.Fatal(err)
				}
			}
			owner := "local-dev"
			if tc == "authentication" {
				owner = "imposter"
			}
			_, err = m.Acquire(ctx, issue.Number, owner, "mock", "plan", time.Hour, 1)
			var refusal *ClaimBaseApprovalError
			if err == nil || errors.As(err, &refusal) {
				t.Fatalf("expected original untyped error: %v", err)
			}
			if tc == "resumed stale" && !errors.Is(err, ErrApprovalRequired) {
				t.Fatal(err)
			}
			if tc == "active" && !errors.Is(err, ErrAlreadyClaimed) {
				t.Fatal(err)
			}
		})
	}
}
