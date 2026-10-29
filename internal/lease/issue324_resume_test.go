package lease

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/tracker"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func issue324Fixture(t *testing.T) (*Manager, *tracker.LocalTracker, *IssueState, string) {
	t.Helper()
	return issue324FixtureCheckpoint(t, true)
}
func issue324FixtureCheckpoint(t *testing.T, record bool) (*Manager, *tracker.LocalTracker, *IssueState, string) {
	t.Helper()
	m, tr, _ := testManager(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := fixtureGitCommand(t, append([]string{"-C", m.repoRoot}, args...)...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	git("init", "--bare", remote)
	git("remote", "add", "origin", remote)
	branch := git("branch", "--show-current")
	git("push", "origin", branch)
	iss, err := tr.CreateIssue(t.Context(), "fresh", testWorkUnit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApproveTask(t.Context(), iss.Number, "local-dev"); err != nil {
		t.Fatal(err)
	}
	st, err := m.Acquire(t.Context(), iss.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", iss.Title, "origin", st.Branch, false)
	if err != nil {
		t.Fatal(err)
	}

	if record {
		st, err = m.RecordCheckpoint(t.Context(), iss.Number, "local-dev", st.Branch, st.BaseSHA)
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.resumeMarker == nil {
		t.Fatal("production allocation/approval did not activate")
	}
	return m, tr, st, path
}

func TestIssue324StaticReceipt(t *testing.T) {
	m, _, st, path := issue324Fixture(t)
	n := st.Lease.IssueNumber
	receipt := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(m.repoRoot, ".swarm/local-tracker/state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Receipts json.RawMessage `json:"resume_receipts"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return string(doc.Receipts)
	}
	before := receipt()
	got, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false)
	if err != nil || got != path {
		t.Fatalf("admission: %s %v", got, err)
	}
	if _, err := m.Renew(t.Context(), n, "local-dev", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(t.Context(), n, "local-dev", "retry"); err != nil {
		t.Fatal(err)
	}
	st, err = m.Acquire(t.Context(), n, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err != nil {
		t.Fatal(err)
	}
	if after := receipt(); after != before {
		t.Fatal("ordinary state writes changed static receipt")
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), st.resumeMarker.Lifecycle) || strings.Contains(string(b), "_local_resume") {
		t.Fatal("private marker leaked into public state")
	}
	// Legacy serialization dropping the private marker never carries permission.
	_, rev, err := m.store.Load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.store.CompareAndSwap(t.Context(), n, rev, b); err != nil {
		t.Fatal(err)
	}
	st, _, err = m.load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
		t.Fatal("missing private marker granted admission")
	}
}

func TestIssue324PublicationAttempt(t *testing.T) {
	m, _, st, _ := issue324Fixture(t)
	n := st.Lease.IssueNumber
	m.BindClaim(n, "stale")
	if err := m.BeginPublicationAttempt(t.Context(), n, "local-dev"); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale: %v", err)
	}
	m.BindClaim(n, st.Lease.ClaimID)
	if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err != nil {
		t.Fatal("stale call revoked active lifecycle:", err)
	}
	if err := m.BeginPublicationAttempt(t.Context(), n, "local-dev"); err != nil {
		t.Fatal(err)
	}
	// There need not be a successful push, PR, or submission record.
	restarted := NewManager(m.tracker, m.security)
	if err := restarted.ConfigureStore(m.repoRoot); err != nil {
		t.Fatal(err)
	}
	restarted.BindClaim(n, st.Lease.ClaimID)
	if _, _, err := restarted.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
		t.Fatal("publication attempt lost durable denial")
	}
}

func TestIssue324MonotonicDenial(t *testing.T) {
	for _, action := range []string{"remote", "store", "git-store-gateway", "upstream", "gateway", "discard"} {
		t.Run(action, func(t *testing.T) {
			m, _, st, _ := issue324Fixture(t)
			n := st.Lease.IssueNumber
			switch action {
			case "remote":
				if _, err := checkpointGit(t.Context(), m.repoRoot, "push", "origin", st.HeadSHA+":refs/heads/"+st.Branch); err != nil {
					t.Fatal(err)
				}
				if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err != nil {
					t.Fatal("matching published checkpoint should use strict path:", err)
				}
				if _, err := checkpointGit(t.Context(), m.repoRoot, "push", "origin", ":refs/heads/"+st.Branch); err != nil {
					t.Fatal(err)
				}
			case "store":
				m.SetStore(m.repoRoot, coordination.NewFileStore(m.repoRoot))
			case "git-store-gateway":
				original := m.Store()
				m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
				m.SetWorkerGateway(issue324Gateway{})
				m.SetWorkerGateway(nil)
				m.SetStore(m.repoRoot, original)
			case "upstream":
				m.SetUpstreamRemote("other")
				m.SetUpstreamRemote("origin")
			case "gateway":
				m.SetWorkerGateway(issue324Gateway{})
				m.SetWorkerGateway(nil)
			case "discard":
				if _, err := m.Release(t.Context(), n, "local-dev", "discard"); err != nil {
					t.Fatal(err)
				}
				if _, err := m.DiscardCheckpoint(t.Context(), n, "local-dev", st.HeadSHA); err != nil {
					t.Fatal(err)
				}
				return
			}
			if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
				t.Fatal("denial was reversed")
			}
		})
	}
}

func TestIssue324LegacyDeletedState(t *testing.T) {
	m, tr, st, _ := issue324Fixture(t)
	n := st.Lease.IssueNumber
	if err := os.Remove(filepath.Join(m.repoRoot, ".swarm/coordination", "issue-"+intString324(n)+".json")); err != nil {
		t.Fatal(err)
	}
	approved, err := m.ApproveTask(t.Context(), n, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if approved.resumeMarker != nil {
		t.Fatal("state deletion reactivated consumed receipt")
	}
	if err := tr.WithResumeReceipt(n, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
		if r == nil || r.Status != "active" {
			t.Fatal("retained active evidence lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func intString324(n int) string { return strconv.Itoa(n) }

func TestIssue324AdmissionFence(t *testing.T) {
	m, _, st, path := issue324Fixture(t)
	n := st.Lease.IssueNumber
	if _, err := m.Renew(t.Context(), n, "local-dev", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); !errors.Is(err, ErrClaimFence) {
		t.Fatalf("stale captured revision: %v", err)
	}
	current, _, err := m.load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "foreign.txt"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ProvisionIssueWorkspace(t.Context(), current, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
		t.Fatal("dirty candidate admitted")
	}
	if b, err := os.ReadFile(filepath.Join(path, "foreign.txt")); err != nil || string(b) != "preserve" {
		t.Fatal("source altered")
	}
}

// Configuration transitions invalidate receipts outside the manager's claim map.
type issue324Gateway struct{}

func (issue324Gateway) Apply(context.Context, ForkRequest) (*IssueState, error) {
	return nil, errors.New("unexpected gateway call")
}

func TestIssue324ActivationCrash(t *testing.T) {
	for _, stage := range []string{"consumed", "marked"} {
		t.Run(stage, func(t *testing.T) {
			m, tr, _, _ := issue324Fixture(t)
			iss, err := tr.CreateIssue(t.Context(), "interrupted", testWorkUnit(), nil)
			if err != nil {
				t.Fatal(err)
			}
			m.resumeApprovalHook = func(at string) error {
				if at == stage {
					return errors.New("approval interrupted")
				}
				return nil
			}
			if _, err := m.ApproveTask(t.Context(), iss.Number, "local-dev"); err == nil {
				t.Fatal("crash hook not reached")
			}
			restarted := NewManager(tr, m.security)
			if err := restarted.ConfigureStore(m.repoRoot); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.ApproveTask(t.Context(), iss.Number, "local-dev"); err != nil {
				t.Fatal(err)
			}
			if err := tr.WithResumeReceipt(iss.Number, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
				if r == nil || r.Status != "consumed" {
					t.Fatalf("restart completed activation: %+v", r)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssue324DeniedWriteFailureStopsPublication(t *testing.T) {
	m, _, st, _ := issue324Fixture(t)
	state := filepath.Join(m.repoRoot, ".swarm/local-tracker/state.json")
	backup := state + ".backup"
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, state); err != nil {
		t.Fatal(err)
	}
	if err := m.BeginPublicationAttempt(t.Context(), st.Lease.IssueNumber, "local-dev"); err == nil {
		t.Fatal("failed authority persistence allowed publication")
	}
}

func TestIssue324AdmissionLocksAndExpiry(t *testing.T) {
	for _, mode := range []string{"revision before lock", "state lock", "denial lock", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			m, tr, st, _ := issue324Fixture(t)
			n := st.Lease.IssueNumber
			if mode == "revision before lock" {
				m.resumeAdmissionHook = func(stage string) {
					if stage == "captured" {
						if _, err := m.Renew(t.Context(), n, "local-dev", time.Hour); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); !errors.Is(err, ErrClaimFence) {
					t.Fatalf("revision race: %v", err)
				}
				return
			}
			if mode == "expiry" {
				m.resumeAdmissionHook = func(stage string) {
					if stage == "inspecting" {
						m.nowFn = func() time.Time { return st.Lease.ExpiresAt.Add(time.Second) }
					}
				}
				if _, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); !errors.Is(err, ErrClaimFence) {
					t.Fatalf("expiry race: %v", err)
				}
				return
			}
			inspecting, release := make(chan struct{}), make(chan struct{})
			m.resumeAdmissionHook = func(stage string) {
				if stage == "inspecting" {
					close(inspecting)
					<-release
				}
			}
			admitted := make(chan error, 1)
			go func() {
				_, _, err := m.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false)
				admitted <- err
			}()
			<-inspecting
			mutation := make(chan error, 1)
			go func() {
				if mode == "state lock" {
					_, err := m.Renew(t.Context(), n, "local-dev", time.Hour)
					mutation <- err
				} else {
					mutation <- tr.DenyResume(n)
				}
			}()
			select {
			case err := <-mutation:
				t.Fatalf("mutation crossed admission lock: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-admitted; err != nil {
				t.Fatal(err)
			}
			if err := <-mutation; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssue324MissingProof(t *testing.T) {
	for _, corruption := range []string{"legacy receipt map", "future version", "foreign authority", "missing receipt", "missing marker"} {
		t.Run(corruption, func(t *testing.T) {
			m, _, st, _ := issue324Fixture(t)
			n := st.Lease.IssueNumber
			if corruption == "missing marker" {
				st.resumeMarker = nil
				if _, err := m.save(t.Context(), n, mustRevision324(t, m, n), st); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(m.repoRoot, ".swarm/local-tracker/state.json")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(b, &doc); err != nil {
					t.Fatal(err)
				}
				var receipts map[int]*tracker.ResumeReceipt
				if err := json.Unmarshal(doc["resume_receipts"], &receipts); err != nil {
					t.Fatal(err)
				}
				switch corruption {
				case "legacy receipt map":
					delete(doc, "resume_receipts")
				case "future version":
					receipts[n].Version = 999
				case "foreign authority":
					receipts[n].Authority = strings.Repeat("0", 64)
				case "missing receipt":
					delete(receipts, n)
				}
				if corruption != "legacy receipt map" {
					doc["resume_receipts"], _ = json.Marshal(receipts)
				}
				b, _ = json.Marshal(doc)
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			current, _, err := m.load(t.Context(), n)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.ProvisionIssueWorkspace(t.Context(), current, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
				t.Fatal("unproven lifecycle admitted")
			}
		})
	}
}
func mustRevision324(t *testing.T, m *Manager, n int) string {
	t.Helper()
	_, rev, err := m.store.Load(t.Context(), n)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestIssue324FirstClaimBaseAdvancementDenies(t *testing.T) {
	m, tr, _, _ := issue324Fixture(t)
	issue, err := tr.CreateIssue(t.Context(), "advance", strings.Replace(testWorkUnit(), "gate: human", "gate: agent", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := m.ApproveTask(t.Context(), issue.Number, "local-dev")
	if err != nil {
		t.Fatal(err)
	}
	if approved.resumeMarker == nil {
		t.Fatal("fresh approval not enrolled")
	}
	if err := os.WriteFile(filepath.Join(m.repoRoot, "advanced.txt"), []byte("base advanced"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "advanced.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "advance base"}, {"push", "origin", "HEAD"}} {
		cmd := fixtureGitCommand(t, append([]string{"-C", m.repoRoot}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, b)
		}
	}
	claimed, err := m.Acquire(t.Context(), issue.Number, "local-dev", "mock", "plan", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.BaseSHA == approved.BaseSHA {
		t.Fatal("base did not advance")
	}
	if err := tr.WithResumeReceipt(issue.Number, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
		if r == nil || r.Status != "denied" || r.Scope != approved.resumeMarker.Scope {
			t.Fatalf("base rebound receipt: %+v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue324CrossRootAuthorityRefused(t *testing.T) {
	source, tr, _, _ := issue324Fixture(t)
	target := filepath.Join(t.TempDir(), "clone")
	remote, err := checkpointGit(t.Context(), source.repoRoot, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	cmd := fixtureGitCommand(t, "clone", remote, target)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, b)
	}
	issue, err := tr.CreateIssue(t.Context(), "cross-root", testWorkUnit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	other := NewManager(tr, source.security)
	if err := other.ConfigureStore(target); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ApproveTask(t.Context(), issue.Number, "local-dev"); err == nil {
		t.Fatal("tracker at source enrolled target FileStore")
	}
	if err := tr.WithResumeReceipt(issue.Number, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
		if r == nil || r.Status != "created" {
			t.Fatalf("failed enrollment consumed receipt: %+v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.denyResume(issue.Number); err != nil {
		t.Fatal(err)
	}
	if err := tr.WithResumeReceipt(issue.Number, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
		if r == nil || r.Status != "denied" {
			t.Fatalf("denial missed actual tracker authority: %+v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type issue324GitHubTracker struct{ *tracker.LocalTracker }

func (issue324GitHubTracker) BackendName() string { return "github" }

func TestIssue324InitialConfigurationIsReadOnly(t *testing.T) {
	source, tr, st, _ := issue324Fixture(t)
	receiptBytes := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(source.repoRoot, ".swarm/local-tracker/state.json"))
		if err != nil {
			t.Fatal(err)
		}
		coord, err := os.ReadFile(filepath.Join(source.repoRoot, ".swarm/coordination", "issue-"+strconv.Itoa(st.Lease.IssueNumber)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + string(coord)
	}
	before := receiptBytes()
	github := NewManager(issue324GitHubTracker{tr}, source.security)
	if err := github.ConfigureStore(source.repoRoot); err != nil {
		t.Fatal(err)
	}
	if err := github.ConfigureStore(source.repoRoot); err != nil {
		t.Fatal(err)
	}
	gateway := NewManager(tr, source.security)
	gateway.SetWorkerGateway(issue324Gateway{})
	if err := gateway.ConfigureStore(source.repoRoot); err != nil {
		t.Fatal(err)
	}
	production := NewManager(issue324GitHubTracker{tr}, source.security)
	production.SetUpstreamRemote("origin")
	production.SetStore(source.repoRoot, coordination.NewGitStore(source.repoRoot))
	production.SetWorkerGateway(issue324Gateway{})
	if receiptBytes() != before {
		t.Fatal("initial read-only authority configuration revoked receipt")
	}
	if _, _, err := source.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err != nil {
		t.Fatal(err)
	}
	// The first real gateway mutation revokes before calling the gateway.
	if _, err := gateway.Acquire(t.Context(), st.Lease.IssueNumber, "local-dev", "mock", "plan", time.Hour, 1); err == nil {
		t.Fatal("fixture gateway should fail")
	}
	if _, _, err := source.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
		t.Fatal("actual authority transition left receipt active")
	}
}

func TestIssue324IncompatibleFirstCheckpointDenies(t *testing.T) {
	m, tr, st, _ := issue324FixtureCheckpoint(t, false)
	unrelated, err := checkpointGit(t.Context(), m.repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", st.BaseSHA+"^{tree}", "-m", "unrelated checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RecordCheckpoint(t.Context(), st.Lease.IssueNumber, "local-dev", st.Branch, unrelated); err == nil {
		t.Fatal("unrelated first checkpoint accepted")
	}
	if err := tr.WithResumeReceipt(st.Lease.IssueNumber, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
		if r == nil || r.Status != "denied" {
			t.Fatalf("incompatible checkpoint did not revoke receipt: %+v", r)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, _, err := m.load(t.Context(), st.Lease.IssueNumber)
	if err != nil {
		t.Fatal(err)
	}
	if current.HeadSHA != "" {
		t.Fatal("failed checkpoint changed authoritative head")
	}
}

func TestIssue324InitialGatewayPublicationBarrier(t *testing.T) {
	source, tr, st, _ := issue324Fixture(t)
	gateway := NewManager(tr, source.security)
	gateway.SetWorkerGateway(issue324Gateway{})
	if err := gateway.ConfigureStore(source.repoRoot); err != nil {
		t.Fatal(err)
	}
	gateway.BindClaim(st.Lease.IssueNumber, st.Lease.ClaimID)
	if err := gateway.BeginPublicationAttempt(t.Context(), st.Lease.IssueNumber, "local-dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
		t.Fatal("gateway publication attempt retained admission")
	}
}

type issue324GatewayFunc func(context.Context, ForkRequest) (*IssueState, error)

func (f issue324GatewayFunc) Apply(ctx context.Context, req ForkRequest) (*IssueState, error) {
	return f(ctx, req)
}

func TestIssue324GatewayFailureOrdering(t *testing.T) {
	for _, failure := range []string{"dispatch", "dispatch-cas", "denial-write"} {
		t.Run(failure, func(t *testing.T) {
			m, tr, st, _ := issue324Fixture(t)
			g := NewManager(issue324GitHubTracker{tr}, m.security)
			g.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
			calls := 0
			g.SetWorkerGateway(issue324GatewayFunc(func(context.Context, ForkRequest) (*IssueState, error) {
				calls++
				if err := tr.WithResumeReceipt(st.Lease.IssueNumber, func(r *tracker.ResumeReceipt, _ *tracker.Issue, _ func() error) error {
					if r == nil || r.Status != "denied" {
						t.Fatalf("request preceded durable denial: %+v", r)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if failure == "dispatch-cas" {
					_, err := m.Store().CompareAndSwap(t.Context(), st.Lease.IssueNumber, "stale-revision", []byte("{}"))
					if err == nil {
						t.Fatal("stale CAS succeeded")
					}
					return nil, err
				}
				return nil, errors.New("dispatch failed after denial")
			}))
			state := filepath.Join(m.repoRoot, ".swarm/local-tracker/state.json")
			if failure == "denial-write" {
				if err := os.Rename(state, state+".backup"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(state+".backup", state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := g.Acquire(t.Context(), st.Lease.IssueNumber, "local-dev", "mock", "plan", time.Hour, 1); err == nil {
				t.Fatal("failed request succeeded")
			}
			if failure == "denial-write" {
				if calls != 0 {
					t.Fatal("forwarded after failed denial")
				}
				old := m.Store()
				m.SetStore(m.repoRoot, coordination.NewGitStore(m.repoRoot))
				m.SetWorkerGateway(issue324Gateway{})
				if m.Store() != old || m.HasWorkerGateway() || m.configErr == nil {
					t.Fatal("failed denial changed configuration")
				}
				return
			}
			if calls != 1 {
				t.Fatalf("dispatch calls: %d", calls)
			}
			g.SetWorkerGateway(nil)
			restarted := NewManager(tr, m.security)
			if err := restarted.ConfigureStore(m.repoRoot); err != nil {
				t.Fatal(err)
			}
			restarted.BindClaim(st.Lease.IssueNumber, st.Lease.ClaimID)
			if _, _, err := restarted.ProvisionIssueWorkspace(t.Context(), st, "local-dev", "fresh", "origin", st.Branch, false); err == nil {
				t.Fatal("failed dispatch restored receipt after restart")
			}
		})
	}
}

func TestIssue324CanonicalAdmissionFence(t *testing.T) {
	for _, change := range []string{"owner", "successor", "task", "policy"} {
		t.Run(change, func(t *testing.T) {
			m, _, captured, _ := issue324Fixture(t)
			n := captured.Lease.IssueNumber
			current, rev, err := m.load(t.Context(), n)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "owner":
				current.Lease.Owner = "other"
			case "successor":
				current.Lease.ClaimID = "successor"
			case "task":
				current.TaskDigest = "changed"
			case "policy":
				current.PolicyDigest = "changed"
			}
			b, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.store.CompareAndSwap(t.Context(), n, rev, b); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(m.repoRoot, ".swarm/local-tracker/state.json")
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.ProvisionIssueWorkspace(t.Context(), captured, "local-dev", "fresh", "origin", captured.Branch, false); err == nil {
				t.Fatal("changed canonical authority admitted")
			}
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("failed admission revoked successor authority")
			}
		})
	}
}
