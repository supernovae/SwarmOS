package coordination

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
)

type revokeFixture struct {
	t       *testing.T
	remote  string
	clone   string
	now     time.Time
	expires string
}

func newRevokeFixture(t *testing.T, remoteName string) *revokeFixture {
	t.Helper()
	root := t.TempDir()
	f := &revokeFixture{t: t, remote: filepath.Join(root, "upstream.git"), clone: filepath.Join(root, "clone"), now: time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)}
	f.expires = f.now.Add(time.Hour).Format(time.RFC3339)
	f.git(root, "init", "--bare", "--initial-branch=main", f.remote)
	f.git(root, "clone", "--origin", remoteName, f.remote, f.clone)
	f.git(f.clone, "config", "user.name", "Revoke Test")
	f.git(f.clone, "config", "user.email", "test@example.invalid")
	f.git(f.clone, "commit", "--allow-empty", "-m", "initial")
	f.git(f.clone, "push", remoteName, "main")
	return f
}

func (f *revokeFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *revokeFixture) remoteRef(ref string) string {
	f.t.Helper()
	out, err := exec.Command("git", "--git-dir="+f.remote, "rev-parse", "--verify", "--quiet", ref).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (f *revokeFixture) state(issue int, claimID, owner, status, forkRepo string) []byte {
	return []byte(fmt.Sprintf(`{"fork_repo":%q,"head_sha":"","lease":{"issue_number":%d,"claim_id":%q,"owner":%q,"status":%q,"expires_at":%q},"last_fork_request_id":7}`, forkRepo, issue, claimID, owner, status, f.expires))
}

// forkClaim acquires issue as owner through the fork coordinator's indexed store.
func (f *revokeFixture) forkClaim(base *GitStore, owner string, issue, maxActive int, requestID int64) (string, error) {
	f.t.Helper()
	rev, err := base.CompareAndSwap(context.Background(), issue, "", []byte(fmt.Sprintf(`{"lease":{"issue_number":%d,"status":"draft"}}`, issue)))
	if err != nil {
		f.t.Fatal(err)
	}
	s := NewForkIndexedStoreWithRemote(f.clone, base.Remote(), owner, maxActive, requestID)
	s.SetNowFunc(func() time.Time { return f.now })
	active := fmt.Sprintf(`{"fork_repo":"%s/fork","lease":{"issue_number":%d,"claim_id":"c%d","owner":%q,"status":"active","expires_at":%q}}`, owner, issue, issue, owner, f.expires)
	return s.CompareAndSwap(context.Background(), issue, rev, []byte(active))
}

func (f *revokeFixture) index(ref string) contributorIndex {
	f.t.Helper()
	out, err := exec.Command("git", "--git-dir="+f.remote, "show", ref+":index.json").CombinedOutput()
	if err != nil {
		f.t.Fatalf("read index: %v: %s", err, out)
	}
	var idx contributorIndex
	if err := json.Unmarshal(out, &idx); err != nil {
		f.t.Fatal(err)
	}
	return idx
}

func revokedFrom(t *testing.T, body []byte) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	raw["lease"].(map[string]any)["status"] = "revoked"
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRevokeForkClaimRemovesContributorIndexEntryAtomically(t *testing.T) {
	ctx := context.Background()
	f := newRevokeFixture(t, "upstream")
	base := NewGitStoreWithRemote(f.clone, "upstream")
	for issue := 1; issue <= 2; issue++ {
		if _, err := f.forkClaim(base, "Alice", issue, 2, int64(100+issue)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.forkClaim(base, "Alice", 3, 2, 103); !errors.Is(err, ErrForkClaimLimit) {
		t.Fatalf("expected limit before revoke, got %v", err)
	}
	ref := "refs/heads/swarm-contributors/alice"
	if got := f.index(ref).Claims; len(got) != 2 {
		t.Fatalf("index before revoke: %+v", got)
	}
	body, rev, err := base.Load(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.CompareAndSwapAndRevokeClaim(ctx, 1, rev, "c1", revokedFrom(t, body)); err != nil {
		t.Fatal(err)
	}
	idx := f.index(ref)
	if len(idx.Claims) != 1 || idx.Claims[0] != (contributorIndexedClaim{IssueNumber: 2, ClaimID: "c2"}) || idx.Actor != "alice" {
		t.Fatalf("index after revoke: %+v", idx)
	}
	after, _, err := base.Load(ctx, 1)
	if err != nil || !strings.Contains(string(after), `"status":"revoked"`) {
		t.Fatalf("state after revoke: %s %v", after, err)
	}
	// Freed capacity admits a new claim under the same max_active_claims.
	if _, err := f.forkClaim(base, "Alice", 3, 2, 104); err != nil {
		t.Fatalf("revoked claim still counted: %v", err)
	}
}

func TestRevokeClaimRejectsStaleRevisionAndReplacementState(t *testing.T) {
	ctx := context.Background()
	f := newRevokeFixture(t, "origin")
	store := NewGitStore(f.clone)
	rev, err := store.CompareAndSwap(ctx, 4, "", f.state(4, "old", "bob", "active", ""))
	if err != nil {
		t.Fatal(err)
	}
	for name, next := range map[string][]byte{
		"still active":    f.state(4, "old", "bob", "active", ""),
		"other claim":     f.state(4, "new", "bob", "revoked", ""),
		"other owner":     f.state(4, "old", "mallory", "revoked", ""),
		"fork changed":    f.state(4, "old", "bob", "revoked", "bob/fork"),
		"replay reset":    []byte(`{"lease":{"issue_number":4,"claim_id":"old","owner":"bob","status":"revoked"},"last_fork_request_id":0}`),
		"malformed state": []byte(`{`),
	} {
		if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 4, rev, "old", next); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 4, rev, "other", f.state(4, "other", "bob", "revoked", "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong captured claim: %v", err)
	}
	// A successor replaced the claim after the maintainer captured it.
	successor, err := store.CompareAndSwap(ctx, 4, rev, f.state(4, "successor", "carol", "active", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 4, rev, "old", f.state(4, "old", "bob", "revoked", "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revoke: %v", err)
	}
	if got := f.remoteRef("refs/heads/swarm-state/issue-4"); got != successor {
		t.Fatalf("stale revoke moved state to %s", got)
	}
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 4, "", "old", f.state(4, "old", "bob", "revoked", "")); err == nil {
		t.Fatal("revoke without captured revision")
	}
}

func TestRevokeForkClaimIndexEdgeCases(t *testing.T) {
	ctx := context.Background()
	f := newRevokeFixture(t, "origin")
	store := NewGitStore(f.clone)

	// Absent index: the revoke still ends the claim.
	rev, err := store.CompareAndSwap(ctx, 5, "", f.state(5, "c5", "dana", "active", "dana/fork"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 5, rev, "c5", f.state(5, "c5", "dana", "revoked", "dana/fork")); err != nil {
		t.Fatalf("absent index: %v", err)
	}
	if f.remoteRef("refs/heads/swarm-contributors/dana") != "" {
		t.Fatal("revoke created an index")
	}

	// Malformed index fails closed without moving either ref.
	rev, err = store.CompareAndSwap(ctx, 6, "", f.state(6, "c6", "erin", "active", "erin/fork"))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := store.commitBlob(ctx, "index.json", []byte("{"), "", "bad index")
	if err != nil {
		t.Fatal(err)
	}
	f.git(f.clone, "push", "origin", bad+":refs/heads/swarm-contributors/erin")
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 6, rev, "c6", f.state(6, "c6", "erin", "revoked", "erin/fork")); err == nil {
		t.Fatal("malformed index accepted")
	}
	if f.remoteRef("refs/heads/swarm-state/issue-6") != rev || f.remoteRef("refs/heads/swarm-contributors/erin") != bad {
		t.Fatal("malformed index revoke moved a ref")
	}
}

func TestRevokeForkClaimOneRefRejectionMovesNeither(t *testing.T) {
	ctx := context.Background()
	f := newRevokeFixture(t, "origin")
	store := NewGitStore(f.clone)
	if _, err := f.forkClaim(store, "frank", 7, 1, 107); err != nil {
		t.Fatal(err)
	}
	indexRef := "refs/heads/swarm-contributors/frank"
	stateRef := "refs/heads/swarm-state/issue-7"
	beforeIndex, beforeState := f.remoteRef(indexRef), f.remoteRef(stateRef)
	hook := filepath.Join(f.remote, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncase \"$1\" in refs/heads/swarm-contributors/*) exit 1;; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, rev, err := store.Load(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapAndRevokeClaim(ctx, 7, rev, "c7", revokedFrom(t, body)); err == nil {
		t.Fatal("rejected index push reported success")
	}
	if f.remoteRef(indexRef) != beforeIndex || f.remoteRef(stateRef) != beforeState {
		t.Fatal("atomic revoke push advanced one ref")
	}
}
