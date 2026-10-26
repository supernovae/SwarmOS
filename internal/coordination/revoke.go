package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ClaimRevokingStore ends one captured claim for a repository maintainer. A
// fork claim leaves its contributor's active-claim index in the same atomic
// push, so it stops counting toward fork.max_active_claims.
type ClaimRevokingStore interface {
	CompareAndSwapAndRevokeClaim(ctx context.Context, n int, old, claimID string, next []byte) (string, error)
}

type revokeStateEnvelope struct {
	ForkRepo string `json:"fork_repo"`
	Lease    struct {
		IssueNumber int    `json:"issue_number"`
		ClaimID     string `json:"claim_id"`
		Owner       string `json:"owner"`
		Status      string `json:"status"`
	} `json:"lease"`
	LastForkRequestID int64 `json:"last_fork_request_id"`
}

// CompareAndSwapAndRevokeClaim replaces the state at old with next only while
// old still holds claimID as its active claim. The contributor index owner
// comes from the authoritative old state, never from the revoking actor.
func (s *GitStore) CompareAndSwapAndRevokeClaim(ctx context.Context, n int, old, claimID string, next []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if old == "" || claimID == "" {
		return "", fmt.Errorf("revoke requires the captured state revision and claim ID")
	}
	prior, err := s.revokeEnvelopeAt(ctx, old)
	if err != nil {
		return "", err
	}
	if prior.Lease.Status != "active" || prior.Lease.ClaimID != claimID {
		return "", fmt.Errorf("%w: claim %s is not the active claim", ErrConflict, claimID)
	}
	var after revokeStateEnvelope
	if err := json.Unmarshal(next, &after); err != nil {
		return "", fmt.Errorf("decode revoked state: %w", err)
	}
	if after.Lease.Status == "active" || after.Lease.ClaimID != claimID || after.Lease.Owner != prior.Lease.Owner ||
		after.Lease.IssueNumber != prior.Lease.IssueNumber || after.ForkRepo != prior.ForkRepo || after.LastForkRequestID != prior.LastForkRequestID {
		return "", fmt.Errorf("revoked state must end claim %s without replacing it", claimID)
	}
	stateCommit, err := s.commitBlob(ctx, "state.json", next, old, "swarm claim revoked")
	if err != nil {
		return "", err
	}
	stateRef := s.ref(n)
	stateLease := "--force-with-lease=" + stateRef + ":" + old
	stateMoved := func() bool {
		actual, readErr := s.remoteSHA(ctx, stateRef)
		return (readErr == nil && actual != old) || errors.Is(readErr, ErrNotFound)
	}
	if prior.ForkRepo == "" {
		if _, err := s.run(ctx, "", "push", "--atomic", s.Remote(), stateLease, stateCommit+":"+stateRef); err != nil {
			if stateMoved() {
				return "", ErrConflict
			}
			return "", fmt.Errorf("push revoked state: %w", err)
		}
		return stateCommit, nil
	}
	contribRef, err := contributorIndexRef(prior.Lease.Owner)
	if err != nil {
		return "", err
	}
	for range 4 {
		idx, oldIndexSHA, err := s.loadContributorIndex(ctx, contribRef)
		if err != nil {
			return "", err
		}
		var kept []contributorIndexedClaim
		for _, entry := range idx.Claims {
			if entry.IssueNumber != n || entry.ClaimID != claimID {
				kept = append(kept, entry)
			}
		}
		push := []string{"push", "--atomic", s.Remote(), stateLease}
		refspecs := []string{stateCommit + ":" + stateRef}
		if len(kept) != len(idx.Claims) {
			idx.Claims = kept
			idxBytes, err := json.Marshal(idx)
			if err != nil {
				return "", err
			}
			indexCommit, err := s.commitBlob(ctx, "index.json", idxBytes, oldIndexSHA, "swarm contributor claim revoked")
			if err != nil {
				return "", err
			}
			push = append(push, "--force-with-lease="+contribRef+":"+oldIndexSHA)
			refspecs = append(refspecs, indexCommit+":"+contribRef)
		}
		if _, err := s.run(ctx, "", append(push, refspecs...)...); err != nil {
			if stateMoved() {
				return "", ErrConflict
			}
			actualIndex, idxReadErr := s.remoteSHA(ctx, contribRef)
			if (idxReadErr == nil && actualIndex != oldIndexSHA) || (errors.Is(idxReadErr, ErrNotFound) && oldIndexSHA != "") {
				continue
			}
			return "", fmt.Errorf("atomic claim revoke push: %w", err)
		}
		return stateCommit, nil
	}
	return "", ErrConflict
}

func (s *GitStore) revokeEnvelopeAt(ctx context.Context, sha string) (*revokeStateEnvelope, error) {
	if _, err := s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), sha); err != nil {
		return nil, fmt.Errorf("fetch state commit %s: %w", sha, err)
	}
	body, err := s.run(ctx, "", "show", sha+":state.json")
	if err != nil {
		return nil, err
	}
	var env revokeStateEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, fmt.Errorf("decode state envelope: %w", err)
	}
	return &env, nil
}

// loadContributorIndex reads a contributor index as stored. A missing index
// is empty; a malformed one fails closed.
func (s *GitStore) loadContributorIndex(ctx context.Context, contribRef string) (*contributorIndex, string, error) {
	sha, err := s.remoteSHA(ctx, contribRef)
	if errors.Is(err, ErrNotFound) {
		return &contributorIndex{}, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if _, err := s.run(ctx, "", "fetch", "--no-write-fetch-head", s.Remote(), sha); err != nil {
		return nil, "", fmt.Errorf("fetch contributor index %s: %w", contribRef, err)
	}
	body, err := s.run(ctx, "", "show", sha+":index.json")
	if err != nil {
		return nil, "", fmt.Errorf("read contributor index %s: %w", contribRef, err)
	}
	var idx contributorIndex
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		return nil, "", fmt.Errorf("parse contributor index %s: %w", contribRef, err)
	}
	return &idx, sha, nil
}
