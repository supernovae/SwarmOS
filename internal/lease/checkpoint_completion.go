package lease

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/workspace"
)

const completionDirectory = "swarm-checkpoint-completion"
const completionLimit = 4 << 20

// CheckpointCompletion is an authenticated coordinator observation, never a
// worktree input. Callers cannot turn a deserialized value into authority.
type CheckpointCompletion struct {
	Version                                                    int
	Repository                                                 string
	Issue                                                      int
	Claim, Owner, Predecessor, Candidate, Tree, Branch, Report string
	Bindings                                                   map[string]string
	Approval                                                   string
	Artifact                                                   contract.Artifact
}
type completionEnvelope struct {
	Payload json.RawMessage
	MAC     string
}

// completionRoot accepts only the main checkout's ordinary .git directory.
// Workers receive packWorker snapshots, which exclude .git. Reject separate
// git directories and aliases rather than assuming their mount permissions.
func (m *Manager) completionRoot(ctx context.Context, create bool) (*safefs.Root, string, error) {
	common, err := checkpointGit(ctx, m.repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, "", err
	}
	expected := filepath.Join(m.repoRoot, ".git")
	canonical, err := filepath.EvalSymlinks(expected)
	if err != nil || canonical != expected || common != expected {
		return nil, "", fmt.Errorf("completion storage requires canonical coordinator Git metadata")
	}
	root, err := safefs.Open(m.repoRoot)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	git, err := root.Sub(".git", false, 0700)
	if err != nil {
		return nil, "", err
	}
	defer git.Close()
	private, err := git.Sub(completionDirectory, create, 0700)
	return private, hash(common), err
}

func completionName(n int) string { return fmt.Sprintf("issue-%d.json", n) }
func completionApproval(st *IssueState) string {
	var reviews []contract.Artifact
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactReview {
			reviews = append(reviews, a)
		}
	}
	b, _ := json.Marshal(struct {
		By      string
		At      interface{}
		Gate    contract.GateMode
		Reviews []contract.Artifact
	}{st.ApprovedBy, st.ApprovedAt, st.EffectiveGate, reviews})
	return hash(string(b))
}
func completionMAC(key, payload []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}
func strictCompletionJSON(b []byte, dst interface{}) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return fmt.Errorf("trailing completion data")
	}
	return nil
}

// SaveCheckpointCompletion persists PASS only after independently checking the
// current claim, approval and the exact clean candidate/report. No lease/store
// lock is acquired while holding the private storage lock.
func (m *Manager) SaveCheckpointCompletion(ctx context.Context, n int, owner, predecessor, candidate string, art contract.Artifact) (resultErr error) {
	defer func() { resultErr = completionPublicError(resultErr) }()
	st, _, err := m.load(ctx, n)
	if err != nil {
		return err
	}
	if err = m.checkStateClaim(ctx, n, owner, st); err != nil {
		return err
	}
	if art.Type != contract.ArtifactImplementation || art.Verdict != "PASS" || art.Body == "" || len(predecessor) != 40 || len(candidate) != 40 || predecessor == candidate || st.HeadSHA != predecessor {
		return fmt.Errorf("successful implementation checkpoint required")
	}
	c := &CheckpointCompletion{Version: 1, Issue: n, Claim: st.Lease.ClaimID, Owner: owner, Predecessor: predecessor, Candidate: candidate, Branch: st.Branch, Bindings: ReviewBindings(st), Approval: completionApproval(st), Artifact: art}
	c.Tree, err = m.implementationTree(ctx, candidate)
	if err != nil {
		return err
	}
	report, err := safefs.ReadFile(workspace.WorktreePath(m.repoRoot, n), ".swarm/active/IMPLEMENTATION.md", 1<<20)
	if err != nil || len(bytes.TrimSpace(report)) == 0 {
		return fmt.Errorf("completion report unavailable")
	}
	c.Report = hash(string(report))
	if err = m.checkCompletion(ctx, st, c); err != nil {
		return err
	}
	root, repo, err := m.completionRoot(ctx, true)
	if err != nil {
		return err
	}
	defer root.Close()
	c.Repository = repo
	lock, err := root.OpenLock("storage.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = filelock.LockExclusive(lock); err != nil {
		return err
	}
	defer filelock.Unlock(lock)
	// A private directory is exclusively created before the first key write.
	// An interrupted initialization fails closed on subsequent attempts.
	key, err := root.ReadFile("key/material", 32)
	if errors.Is(err, os.ErrNotExist) {
		if err = root.Mkdir("key", 0700); err != nil {
			return fmt.Errorf("completion key initialization interrupted")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		if err = root.WriteFileDurable("key/material", key, 0600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(key) != 32 {
		return fmt.Errorf("invalid completion key")
	}
	if _, err = root.Lstat(completionName(n)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pending completion must be recovered before implementation")
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(completionEnvelope{payload, completionMAC(key, payload)})
	if err != nil {
		return err
	}
	if len(raw) > completionLimit {
		return fmt.Errorf("completion exceeds size limit")
	}
	return root.WriteFileDurable(completionName(n), raw, 0600)
}

// InspectCheckpointCompletion distinguishes absent storage from invalid present
// storage. Invalid records always stop work; they are never silently discarded.
func (m *Manager) InspectCheckpointCompletion(ctx context.Context, n int) (result *CheckpointCompletion, resultErr error) {
	defer func() { resultErr = completionPublicError(resultErr) }()
	root, repo, err := m.completionRoot(ctx, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := root.ReadFile(completionName(n), completionLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pending completion: %w", err)
	}
	key, err := root.ReadFile("key/material", 32)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("pending completion authentication unavailable")
	}
	var envelope completionEnvelope
	if err = strictCompletionJSON(raw, &envelope); err != nil {
		return nil, fmt.Errorf("invalid completion envelope")
	}
	mac, err := hex.DecodeString(envelope.MAC)
	want, _ := hex.DecodeString(completionMAC(key, envelope.Payload))
	if err != nil || !hmac.Equal(mac, want) {
		return nil, fmt.Errorf("invalid completion authentication")
	}
	var c CheckpointCompletion
	if err = strictCompletionJSON(envelope.Payload, &c); err != nil || c.Version != 1 || c.Repository != repo || c.Issue != n {
		return nil, fmt.Errorf("invalid completion identity")
	}
	return &c, nil
}

func (m *Manager) checkCompletion(ctx context.Context, st *IssueState, c *CheckpointCompletion) error {
	if m.gateway != nil || !directIssue(st) || st.DroppedCheckpoint != nil || st.Lease.ClaimID != c.Claim || st.Lease.Owner != c.Owner || st.Branch != c.Branch || (st.HeadSHA != c.Predecessor && st.HeadSHA != c.Candidate) || !reflect.DeepEqual(ReviewBindings(st), c.Bindings) || completionApproval(st) != c.Approval {
		return fmt.Errorf("%w: completion inputs changed", ErrClaimFence)
	}
	if err := m.checkStateClaim(ctx, c.Issue, c.Owner, st); err != nil {
		return err
	}
	issue, err := m.tracker.GetIssue(ctx, c.Issue)
	if err != nil {
		return err
	}
	wu, err := contract.ParseWorkUnitFromIssue(c.Issue, issue.Title, issue.Body)
	if err != nil {
		return err
	}
	if err = VerifyApproval(st, wu); err != nil {
		return err
	}
	if c.Artifact.Type != contract.ArtifactImplementation || c.Artifact.Verdict != "PASS" || c.Artifact.Metadata["head_sha"] != c.Candidate {
		return fmt.Errorf("invalid completion artifact")
	}
	for k, v := range c.Bindings {
		if v == "" || c.Artifact.Metadata[k] != v {
			return fmt.Errorf("invalid completion artifact binding")
		}
	}
	observation := workspace.InspectCheckpoint(ctx, m.repoRoot, ".", c.Issue, c.Branch, st.HeadSHA)
	if observation.Error != "" || observation.Worktree.Error != "" || observation.LocalBranch.Error != "" || observation.Worktree.SHA != c.Candidate || observation.LocalBranch.SHA != c.Candidate || observation.AttachedBranch != c.Branch || observation.Dirty || observation.Rebasing {
		return fmt.Errorf("%w: completion worktree identity changed", ErrClaimFence)
	}
	tree, err := m.implementationTree(ctx, c.Candidate)
	if err != nil || tree != c.Tree {
		return fmt.Errorf("%w: completion tree changed", ErrClaimFence)
	}
	report, err := safefs.ReadFile(workspace.WorktreePath(m.repoRoot, c.Issue), ".swarm/active/IMPLEMENTATION.md", 1<<20)
	if err != nil || hash(string(report)) != c.Report {
		return fmt.Errorf("%w: completion report changed", ErrClaimFence)
	}
	return nil
}

// RecoverCheckpointCompletion rereads authenticated bytes, then uses only the
// existing reconciler. The final artifact eligibility runs inside each fenced
// CAS attempt, independently of the earlier reconciliation observation.
func (m *Manager) RecoverCheckpointCompletion(ctx context.Context, n int, owner string) (result *IssueState, resultErr error) {
	defer func() { resultErr = completionPublicError(resultErr) }()
	c, err := m.InspectCheckpointCompletion(ctx, n)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("missing completion receipt")
	}
	if c.Owner != owner {
		return nil, ErrClaimFence
	}
	st, _, err := m.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if err = m.checkCompletion(ctx, st, c); err != nil {
		return nil, err
	}
	if st.HeadSHA == c.Predecessor {
		p, err := m.PrepareCheckpoint(ctx, n, owner, c.Predecessor, c.Candidate, false)
		if err != nil {
			return nil, err
		}
		if p.ClaimRequired || p.ReviewRequired {
			return nil, ErrClaimFence
		}
		if err = m.checkCompletion(ctx, p.State, c); err != nil {
			return nil, err
		}
		result, err := m.reconcileCheckpoint(ctx, n, owner, c.Predecessor, c.Candidate, false, false, false, p.revision, func(fresh *IssueState) error { return m.completionEligibility(ctx, fresh, c) })
		if err != nil {
			return nil, err
		}
		st = result.State
	}
	if VerifyTrustedImplementation(st, c.Candidate, c.Tree, c.Branch) != nil {
		st, err = m.recordTrustedImplementation(ctx, n, owner, c.Artifact, func(fresh *IssueState) error {
			if fresh.HeadSHA != c.Candidate {
				return ErrClaimFence
			}
			return m.completionEligibility(ctx, fresh, c)
		})
		if err != nil {
			return nil, err
		}
	}
	if err = VerifyTrustedImplementation(st, c.Candidate, c.Tree, c.Branch); err != nil {
		return nil, err
	}
	// Removal follows durable trusted state. Failure keeps an idempotent receipt.
	root, _, err := m.completionRoot(ctx, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	lock, err := root.OpenLock("storage.lock")
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err = filelock.LockExclusive(lock); err != nil {
		return nil, err
	}
	defer filelock.Unlock(lock)
	current, err := m.InspectCheckpointCompletion(ctx, n)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return st, nil
	}
	if !reflect.DeepEqual(current, c) {
		return nil, ErrClaimFence
	}
	if err = root.Remove(completionName(n)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return st, nil
}

func (m *Manager) completionEligibility(ctx context.Context, st *IssueState, c *CheckpointCompletion) error {
	current, err := m.InspectCheckpointCompletion(ctx, c.Issue)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, c) {
		return ErrClaimFence
	}
	return m.checkCompletion(ctx, st, c)
}

// Keep storage paths out of user-visible errors while retaining error identity.
type completionStorageError struct{ cause error }

func (e *completionStorageError) Error() string {
	return "private completion storage unavailable; checkpoint preserved"
}
func (e *completionStorageError) Unwrap() error { return e.cause }
func completionPublicError(err error) error {
	var path *os.PathError
	var link *os.LinkError
	if errors.As(err, &path) || errors.As(err, &link) {
		return &completionStorageError{err}
	}
	return err
}
