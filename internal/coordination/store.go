package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/filelock"
	"github.com/supernovae/swarmos/internal/safefs"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// PublishingStore atomically advances both the authoritative state ref and a
// checkpoint branch. Only GitStore implements it.
type PublishingStore interface {
	CompareAndSwapAndPublish(context.Context, int, string, []byte, string, string) (string, error)
}

// ExpectedCheckpointPublishingStore uses the recorded or captured remote head
// as the publication fence. An empty expectation requires an absent branch.
type ExpectedCheckpointPublishingStore interface {
	CompareAndSwapAndPublishExpected(context.Context, int, string, []byte, string, string, string) (string, error)
}

// ExpectedPRPublishingStore atomically advances a linked PR branch and issue
// state using the caller's captured remote head, never a freshly read value.
type ExpectedPRPublishingStore interface {
	CompareAndSwapAndPublishPR(context.Context, int, string, []byte, string, string, string) (string, error)
}

// RetainingStore advances issue state and permanently retains the named
// checkpoint commits in one fenced write. A retention ref that already exists
// must name exactly its commit; none is ever rewritten.
type RetainingStore interface {
	CompareAndSwapRetaining(ctx context.Context, n int, old string, next []byte, heads ...string) (string, error)
}

var ErrConflict = errors.New("coordination compare-and-swap conflict")
var ErrNotFound = errors.New("coordination state not found")
var ErrReplay = errors.New("fork request comment ID already processed")
var ErrForkClaimLimit = errors.New("fork active claim limit reached for contributor")

type Store interface {
	Load(context.Context, int) ([]byte, string, error)
	CompareAndSwap(context.Context, int, string, []byte) (string, error)
}
type FileStore struct {
	repoRoot string
}

func NewFileStore(root string) *FileStore {
	return &FileStore{repoRoot: root}
}
func (s *FileStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	base, err := safefs.Open(s.repoRoot)
	if err != nil {
		return nil, "", err
	}
	defer base.Close()
	state, err := base.Sub(filepath.Join(".swarm", "coordination"), false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	defer state.Close()
	name := fmt.Sprintf("issue-%d.json", n)
	lock, err := state.OpenLock(name + ".lock")
	if err != nil {
		return nil, "", err
	}
	defer lock.Close()
	if err := filelock.LockExclusive(lock); err != nil {
		return nil, "", err
	}
	defer filelock.Unlock(lock)
	b, err := state.ReadFile(name, 64<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return b, digest(b), nil
}

// LoadCheckpointPreview observes the atomically replaced state without creating
// a lock file. Publication still uses the normal locked CAS.
func (s *FileStore) LoadCheckpointPreview(ctx context.Context, n int) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	b, err := safefs.ReadFile(s.repoRoot, filepath.Join(".swarm", "coordination", fmt.Sprintf("issue-%d.json", n)), 64<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return b, digest(b), nil
}

func (s *FileStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	base, err := safefs.Open(s.repoRoot)
	if err != nil {
		return "", err
	}
	defer base.Close()
	state, err := base.Sub(filepath.Join(".swarm", "coordination"), true, 0700)
	if err != nil {
		return "", err
	}
	defer state.Close()
	name := fmt.Sprintf("issue-%d.json", n)
	f, err := state.OpenLock(name + ".lock")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err = filelock.LockExclusive(f); err != nil {
		return "", err
	}
	defer filelock.Unlock(f)
	current := ""
	if b, err := state.ReadFile(name, 64<<20); err == nil {
		current = digest(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if current != old {
		return "", ErrConflict
	}
	if err = state.WriteFile(name, next, 0600); err != nil {
		return "", err
	}
	return digest(next), nil
}

type GitStore struct {
	root, remote string
	layout       RefLayout
}

// NewGitStore keeps authoritative state on the origin remote.
func NewGitStore(root string) *GitStore { return &GitStore{root: root} }

// NewGitStoreWithRemote keeps authoritative state on the named upstream remote
// in the branch namespace.
func NewGitStoreWithRemote(root, remote string) *GitStore {
	return NewGitStoreWithLayout(root, remote, BranchLayout)
}

// NewGitStoreWithLayout keeps authoritative state on the named upstream remote
// in the ledger namespace the trusted default branch selected.
func NewGitStoreWithLayout(root, remote string, layout RefLayout) *GitStore {
	return &GitStore{root: root, remote: strings.TrimSpace(remote), layout: layout}
}

// Remote names the upstream remote holding authoritative state.
func (s *GitStore) Remote() string {
	if s.remote == "" {
		return "origin"
	}
	return s.remote
}
func (s *GitStore) ref(n int) string { return s.layout.StateRef(n) }
func (s *GitStore) run(ctx context.Context, input string, args ...string) (string, error) {
	c, redact, err := gitcredential.Command(ctx, s.root, args...)
	if err != nil {
		return "", err
	}
	var b []byte
	var e error
	if gitcredential.IsNetwork(args) {
		if input != "" {
			return "", fmt.Errorf("network Git command does not accept input")
		}
		b, e = gitcredential.Network(ctx, s.root, true, args...)
	} else {
		c.Stdin = strings.NewReader(input)
		b, e = c.CombinedOutput()
	}
	if e != nil {
		return "", fmt.Errorf("git %s: %w: %s", redact(strings.Join(args, " ")), e, redact(strings.TrimSpace(string(b))))
	}
	return redact(strings.TrimSpace(string(b))), nil
}

// remoteSHA reads ref and, for ledger refs, its other-namespace counterpart in
// the same request. Any counterpart fails closed instead of being chosen.
func (s *GitStore) remoteSHA(ctx context.Context, ref string) (string, error) {
	refs := []string{ref}
	counterpart, ledger := s.layout.counterpartRef(ref)
	if ledger {
		refs = append(refs, counterpart)
	}
	found, err := s.remoteRefs(ctx, refs...)
	if err != nil {
		return "", err
	}
	sha, ok := found[ref]
	if _, dup := found[counterpart]; ledger && dup {
		return "", s.layout.ledgerConflict(ref, counterpart, ok)
	}
	if !ok {
		return "", ErrNotFound
	}
	return sha, nil
}
func (s *GitStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	ref := s.ref(n)
	sha, err := s.remoteSHA(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	if _, err = s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), sha); err != nil {
		return nil, "", fmt.Errorf("fetch pinned state %s: %w", ref, err)
	}
	body, err := s.run(ctx, "", "show", sha+":state.json")
	if err != nil {
		return nil, "", err
	}
	return []byte(body), sha, nil
}

// LoadCheckpointPreview never fetches. An unavailable pinned state must be
// fetched explicitly by the operator before a read-only preview can inspect it.
func (s *GitStore) LoadCheckpointPreview(ctx context.Context, n int) ([]byte, string, error) {
	sha, err := s.remoteSHA(ctx, s.ref(n))
	if err != nil {
		return nil, "", err
	}
	body, err := s.run(ctx, "", "show", sha+":state.json")
	if err != nil {
		return nil, "", fmt.Errorf("pinned checkpoint state %s unavailable locally; fetch the state ref before retrying: %w", sha, err)
	}
	return []byte(body), sha, nil
}

func (s *GitStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := s.run(ctx, "swarm coordination state\n", args...)
	if err != nil {
		return "", err
	}
	ref := s.ref(n)
	if err := s.checkLedger(ctx, ref); err != nil {
		return "", err
	}
	leaseArg := ref + ":" + old
	if old == "" {
		leaseArg = ref + ":"
	}
	if err = s.push(ctx, "push", s.Remote(), "--force-with-lease="+leaseArg, commit+":"+ref); err != nil {
		if publicationStatus(err) {
			return "", err
		}
		actual, readErr := s.remoteSHA(ctx, ref)
		if readErr == nil && actual != old {
			return "", ErrConflict
		}
		if errors.Is(readErr, ErrNotFound) && old != "" {
			return "", ErrConflict
		}
		return "", fmt.Errorf("push coordination state: %w", err)
	}
	return commit, nil
}
func (s *GitStore) CompareAndSwapAndPublish(ctx context.Context, n int, old string, next []byte, branch, head string) (string, error) {
	return s.publishCheckpoint(ctx, n, old, next, branch, head, nil)
}

func (s *GitStore) CompareAndSwapAndPublishExpected(ctx context.Context, n int, old string, next []byte, branch, head, expected string) (string, error) {
	if expected != "" && len(expected) != 40 {
		return "", fmt.Errorf("invalid retired checkpoint")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", err
	}
	if expected != "" {
		if _, err := s.run(ctx, "", "merge-base", "--is-ancestor", expected, head); err != nil {
			return "", fmt.Errorf("checkpoint recovery must fast-forward: %w", err)
		}
	}
	return s.publishCheckpoint(ctx, n, old, next, branch, head, &expected)
}

// CompareAndSwapAndReconcileCheckpoint permits an explicitly authorized rewrite.
// Unlike recovery, it does not impose fast-forward ancestry; the manager checks
// reviewed lineage before invoking this expected-head transaction.
func (s *GitStore) CompareAndSwapAndReconcileCheckpoint(ctx context.Context, n int, old string, next []byte, branch, head, expected string) (string, error) {
	if len(expected) != 40 {
		return "", fmt.Errorf("invalid predecessor checkpoint")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", err
	}
	return s.publishCheckpoint(ctx, n, old, next, branch, head, &expected)
}

func (s *GitStore) publishCheckpoint(ctx context.Context, n int, old string, next []byte, branch, head string, expected *string) (string, error) {
	if !strings.HasPrefix(branch, "swarm/issue-") || strings.Contains(branch, "..") || head == "" {
		return "", fmt.Errorf("invalid checkpoint branch or head")
	}
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if old != "" {
		args = append(args, "-p", old)
	}
	commit, err := s.run(ctx, "swarm coordination checkpoint\n", args...)
	if err != nil {
		return "", err
	}
	stateRef := s.ref(n)
	if err := s.checkLedger(ctx, stateRef); err != nil {
		return "", err
	}
	branchRef := "refs/heads/" + branch
	oldBranch, branchErr := s.remoteSHA(ctx, branchRef)
	if errors.Is(branchErr, ErrNotFound) {
		oldBranch = ""
	} else if branchErr != nil {
		return "", branchErr
	}
	if expected != nil {
		if oldBranch != *expected {
			return "", fmt.Errorf("%w: remote checkpoint is %s, expected %s", ErrConflict, oldBranch, *expected)
		}
		oldBranch = *expected
	}
	stateLease := stateRef + ":" + old
	if old == "" {
		stateLease = stateRef + ":"
	}
	branchLease := branchRef + ":" + oldBranch
	if oldBranch == "" {
		branchLease = branchRef + ":"
	}
	retention, err := s.historicalRetentionArgs(ctx, n, old, oldBranch, head)
	if err != nil {
		return "", err
	}
	push := append([]string{"push", "--atomic", s.Remote(), "--force-with-lease=" + stateLease, "--force-with-lease=" + branchLease, commit + ":" + stateRef, head + ":" + branchRef}, retention...)
	if err = s.push(ctx, push...); err != nil {
		if publicationStatus(err) {
			return "", err
		}
		if expected != nil {
			return "", fmt.Errorf("%w: atomic checkpoint push: %w", ErrConflict, err)
		}
		return "", fmt.Errorf("atomic checkpoint push: %w", err)
	}
	return commit, nil
}

func (s *GitStore) CompareAndSwapAndPublishPR(ctx context.Context, n int, oldState string, next []byte, branch, expectedHead, head string) (string, error) {
	if oldState == "" || len(expectedHead) != 40 || len(head) != 40 || expectedHead == head || branch == "" || strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("invalid PR publication identity")
	}
	if _, err := hex.DecodeString(expectedHead); err != nil {
		return "", fmt.Errorf("invalid expected PR head: %w", err)
	}
	if _, err := hex.DecodeString(head); err != nil {
		return "", fmt.Errorf("invalid candidate PR head: %w", err)
	}
	branchRef := "refs/heads/" + branch
	if _, err := s.run(ctx, "", "check-ref-format", branchRef); err != nil {
		return "", fmt.Errorf("invalid PR head branch: %w", err)
	}
	if _, err := s.run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
		return "", fmt.Errorf("candidate PR commit unavailable: %w", err)
	}
	blob, err := s.run(ctx, string(next), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\tstate.json\n", blob), "mktree")
	if err != nil {
		return "", err
	}
	commit, err := s.run(ctx, "swarm PR revision published\n", "-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree, "-p", oldState)
	if err != nil {
		return "", err
	}
	stateRef := s.ref(n)
	if err := s.checkLedger(ctx, stateRef); err != nil {
		return "", err
	}
	retention, err := s.historicalRetentionArgs(ctx, n, oldState, expectedHead, head)
	if err != nil {
		return "", err
	}
	push := append([]string{"push", "--atomic", s.Remote(), "--force-with-lease=" + stateRef + ":" + oldState, "--force-with-lease=" + branchRef + ":" + expectedHead, commit + ":" + stateRef, head + ":" + branchRef}, retention...)
	if err = s.push(ctx, push...); err != nil {
		if publicationStatus(err) {
			return "", err
		}
		return "", fmt.Errorf("atomic PR publication rejected (issue state or remote head changed): %w", err)
	}
	return commit, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type contributorIndex struct {
	Version int                       `json:"version"`
	Actor   string                    `json:"actor"`
	Claims  []contributorIndexedClaim `json:"claims,omitempty"`
}

type contributorIndexedClaim struct {
	IssueNumber int    `json:"issue_number"`
	ClaimID     string `json:"claim_id"`
}

type forkStateEnvelope struct {
	HeadSHA string `json:"head_sha"`
	Lease   struct {
		IssueNumber int       `json:"issue_number"`
		ClaimID     string    `json:"claim_id"`
		Owner       string    `json:"owner"`
		Status      string    `json:"status"`
		ExpiresAt   time.Time `json:"expires_at"`
	} `json:"lease"`
	LastForkRequestID int64 `json:"last_fork_request_id"`
}

// ForkIndexedStore wraps an upstream GitStore for verified fork coordinator
// events. It intentionally does not implement PublishingStore.
type ForkIndexedStore struct {
	git       *GitStore
	actor     string
	maxActive int
	requestID int64
	nowFn     func() time.Time
}

func NewForkIndexedStore(root, actor string, maxActive int, requestID int64) *ForkIndexedStore {
	return NewForkIndexedStoreWithRemote(root, "", actor, maxActive, requestID)
}

// NewForkIndexedStoreWithRemote is NewForkIndexedStore on a named upstream remote.
func NewForkIndexedStoreWithRemote(root, remote, actor string, maxActive int, requestID int64) *ForkIndexedStore {
	return NewForkIndexedStoreWithLayout(root, remote, BranchLayout, actor, maxActive, requestID)
}

// NewForkIndexedStoreWithLayout is NewForkIndexedStoreWithRemote in the ledger
// namespace the trusted default branch selected.
func NewForkIndexedStoreWithLayout(root, remote string, layout RefLayout, actor string, maxActive int, requestID int64) *ForkIndexedStore {
	if maxActive <= 0 {
		maxActive = 1
	}
	return &ForkIndexedStore{
		git:       NewGitStoreWithLayout(root, remote, layout),
		actor:     strings.TrimSpace(actor),
		maxActive: maxActive,
		requestID: requestID,
		nowFn:     time.Now,
	}
}

// Layout is the ledger namespace this store reads and writes.
func (s *ForkIndexedStore) Layout() RefLayout { return s.git.layout }

func (s *ForkIndexedStore) SetNowFunc(fn func() time.Time) {
	if fn != nil {
		s.nowFn = fn
	}
}

func (s *ForkIndexedStore) contributorRef() (string, error) {
	return contributorIndexRef(s.git.layout, s.actor)
}

// contributorIndexRef names the normalized per-contributor fork claim index
// in the given ledger namespace.
func contributorIndexRef(layout RefLayout, actor string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(actor))
	if normalized == "" || strings.ContainsAny(normalized, " /\\:~^?*[]@") || strings.Contains(normalized, "..") {
		return "", fmt.Errorf("invalid contributor actor %q", actor)
	}
	return layout.ContributorRef(normalized), nil
}

func (s *ForkIndexedStore) Load(ctx context.Context, n int) ([]byte, string, error) {
	return s.git.Load(ctx, n)
}

func (s *ForkIndexedStore) loadStateAtCommit(ctx context.Context, sha string) (*forkStateEnvelope, error) {
	if sha == "" {
		return &forkStateEnvelope{}, nil
	}
	if _, err := s.git.run(ctx, "", "fetch", "--no-write-fetch-head", s.git.Remote(), sha); err != nil {
		return nil, fmt.Errorf("fetch state commit %s: %w", sha, err)
	}
	body, err := s.git.run(ctx, "", "show", sha+":state.json")
	if err != nil {
		return nil, err
	}
	var env forkStateEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, fmt.Errorf("decode state envelope: %w", err)
	}
	return &env, nil
}

func (s *ForkIndexedStore) stampRequestID(next []byte) ([]byte, *forkStateEnvelope, error) {
	var env forkStateEnvelope
	if err := json.Unmarshal(next, &env); err != nil {
		return nil, nil, fmt.Errorf("decode next state envelope: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(next, &raw); err != nil {
		return nil, nil, fmt.Errorf("decode next state map: %w", err)
	}
	reqBytes, err := json.Marshal(s.requestID)
	if err != nil {
		return nil, nil, err
	}
	raw["last_fork_request_id"] = reqBytes
	env.LastForkRequestID = s.requestID
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	return out, &env, nil
}

func (s *ForkIndexedStore) commitBlob(ctx context.Context, fileName string, content []byte, parent string, msg string) (string, error) {
	return s.git.commitBlob(ctx, fileName, content, parent, msg)
}

func (s *GitStore) commitBlob(ctx context.Context, fileName string, content []byte, parent string, msg string) (string, error) {
	blob, err := s.run(ctx, string(content), "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	tree, err := s.run(ctx, fmt.Sprintf("100644 blob %s\t%s\n", blob, fileName), "mktree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=SwarmOS", "-c", "user.email=swarmos@localhost", "commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return s.run(ctx, msg+"\n", args...)
}

func (s *ForkIndexedStore) loadAndRevalidateIndex(ctx context.Context, contribRef string, currentIssue int) ([]contributorIndexedClaim, string, error) {
	oldSHA, err := s.git.remoteSHA(ctx, contribRef)
	if errors.Is(err, ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if _, err := s.git.run(ctx, "", "fetch", "--no-write-fetch-head", s.git.Remote(), oldSHA); err != nil {
		return nil, "", fmt.Errorf("fetch contributor index %s: %w", contribRef, err)
	}
	body, err := s.git.run(ctx, "", "show", oldSHA+":index.json")
	if err != nil {
		return nil, "", fmt.Errorf("read contributor index %s: %w", contribRef, err)
	}
	var idx contributorIndex
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		return nil, "", fmt.Errorf("parse contributor index %s: %w", contribRef, err)
	}
	now := s.nowFn()
	seen := make(map[int]bool)
	var kept []contributorIndexedClaim
	for _, entry := range idx.Claims {
		if entry.IssueNumber <= 0 || entry.ClaimID == "" || entry.IssueNumber == currentIssue || seen[entry.IssueNumber] {
			continue
		}
		rawState, _, loadErr := s.git.Load(ctx, entry.IssueNumber)
		if errors.Is(loadErr, ErrNotFound) {
			continue
		}
		if loadErr != nil {
			return nil, "", loadErr
		}
		var env forkStateEnvelope
		if err := json.Unmarshal(rawState, &env); err != nil {
			continue
		}
		if env.Lease.Status == "active" &&
			strings.EqualFold(env.Lease.Owner, s.actor) &&
			env.Lease.ClaimID == entry.ClaimID &&
			env.Lease.ExpiresAt.After(now) {
			seen[entry.IssueNumber] = true
			kept = append(kept, contributorIndexedClaim{
				IssueNumber: entry.IssueNumber,
				ClaimID:     entry.ClaimID,
			})
		}
	}
	return kept, oldSHA, nil
}

func (s *ForkIndexedStore) CompareAndSwap(ctx context.Context, n int, old string, next []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.requestID <= 0 {
		return "", fmt.Errorf("fork indexed store requires a positive request ID")
	}
	stateRef := s.git.ref(n)
	if err := s.git.checkLedger(ctx, stateRef); err != nil {
		return "", err
	}
	priorEnv, err := s.loadStateAtCommit(ctx, old)
	if err != nil {
		return "", err
	}
	if priorEnv.LastForkRequestID >= s.requestID {
		return "", fmt.Errorf("%w: request %d <= last processed %d", ErrReplay, s.requestID, priorEnv.LastForkRequestID)
	}
	stampedNext, nextEnv, err := s.stampRequestID(next)
	if err != nil {
		return "", err
	}
	stateCommit, err := s.commitBlob(ctx, "state.json", stampedNext, old, "swarm coordination state")
	if err != nil {
		return "", err
	}
	stateLease := stateRef + ":" + old

	now := s.nowFn()
	wasActiveForActor := priorEnv.Lease.Status == "active" &&
		strings.EqualFold(priorEnv.Lease.Owner, s.actor) &&
		priorEnv.Lease.ClaimID != "" &&
		priorEnv.Lease.ExpiresAt.After(now)
	isActiveForActor := nextEnv.Lease.Status == "active" &&
		strings.EqualFold(nextEnv.Lease.Owner, s.actor) &&
		nextEnv.Lease.ClaimID != "" &&
		nextEnv.Lease.ExpiresAt.After(now)
	isAcquire := isActiveForActor && (!wasActiveForActor || priorEnv.Lease.ClaimID != nextEnv.Lease.ClaimID)
	isLeave := priorEnv.Lease.Status == "active" && nextEnv.Lease.Status != "active"

	if !isAcquire && !isLeave {
		heads := []string{}
		if nextEnv.HeadSHA != priorEnv.HeadSHA {
			heads = append(heads, priorEnv.HeadSHA, nextEnv.HeadSHA)
		}
		retention, err := s.git.historicalRetentionArgs(ctx, n, old, heads...)
		if err != nil {
			return "", err
		}
		push := append([]string{"push", "--atomic", s.git.Remote(), "--force-with-lease=" + stateLease, stateCommit + ":" + stateRef}, retention...)
		if err := s.git.push(ctx, push...); err != nil {
			if publicationStatus(err) {
				return "", err
			}
			actual, readErr := s.git.remoteSHA(ctx, stateRef)
			if readErr == nil && actual != old {
				return "", ErrConflict
			}
			if errors.Is(readErr, ErrNotFound) && old != "" {
				return "", ErrConflict
			}
			return "", fmt.Errorf("push coordination state: %w", err)
		}
		return stateCommit, nil
	}
	contribRef, err := s.contributorRef()
	if err != nil {
		return "", err
	}
	for range 4 {
		kept, oldIndexSHA, err := s.loadAndRevalidateIndex(ctx, contribRef, n)
		if err != nil {
			return "", err
		}
		if isAcquire {
			if len(kept) >= s.maxActive {
				return "", fmt.Errorf("%w: contributor %q already has %d active fork claim(s) (max %d)", ErrForkClaimLimit, s.actor, len(kept), s.maxActive)
			}
			kept = append(kept, contributorIndexedClaim{
				IssueNumber: n,
				ClaimID:     nextEnv.Lease.ClaimID,
			})
		}
		idxBytes, err := json.Marshal(contributorIndex{
			Version: 1,
			Actor:   strings.ToLower(strings.TrimSpace(s.actor)),
			Claims:  kept,
		})
		if err != nil {
			return "", err
		}
		indexCommit, err := s.commitBlob(ctx, "index.json", idxBytes, oldIndexSHA, "swarm contributor claim index")
		if err != nil {
			return "", err
		}
		contribLease := contribRef + ":" + oldIndexSHA
		if err := s.git.push(ctx, "push", "--atomic", s.git.Remote(), "--force-with-lease="+stateLease, "--force-with-lease="+contribLease, stateCommit+":"+stateRef, indexCommit+":"+contribRef); err != nil {
			if publicationStatus(err) {
				return "", err
			}
			actualState, stateReadErr := s.git.remoteSHA(ctx, stateRef)
			if (stateReadErr == nil && actualState != old) || (errors.Is(stateReadErr, ErrNotFound) && old != "") {
				return "", ErrConflict
			}
			actualIndex, idxReadErr := s.git.remoteSHA(ctx, contribRef)
			if (idxReadErr == nil && actualIndex != oldIndexSHA) || (errors.Is(idxReadErr, ErrNotFound) && oldIndexSHA != "") {
				continue
			}
			return "", fmt.Errorf("atomic fork coordination push: %w", err)
		}
		return stateCommit, nil
	}
	return "", ErrConflict
}

func (s *ForkIndexedStore) RequestID() int64 {
	return s.requestID
}

// ErrPublicationUnknown requires authoritative reinspection before retrying.
var ErrPublicationUnknown = errors.New("coordination publication outcome unknown; reload authoritative state before any retry")

func publicationStatus(err error) bool {
	var known *gitcredential.KnownResultCleanupError
	return errors.Is(err, ErrPublicationUnknown) || errors.As(err, &known)
}
func (s *GitStore) push(ctx context.Context, args ...string) error {
	_, err := s.run(ctx, "", args...)
	if errors.Is(err, gitcredential.ErrInterrupted) {
		return fmt.Errorf("%w: %w", ErrPublicationUnknown, err)
	}
	return err
}
