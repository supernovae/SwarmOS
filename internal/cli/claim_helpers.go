package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workspace"
)

// bindPersistedClaim authenticates the actor and binds this machine's persisted
// claim token for the issue, so later mutations are fenced to that claim.
func bindPersistedClaim(ctx context.Context, root string, tr tracker.Tracker, manager *lease.Manager, issue int) (owner, token string, err error) {
	owner, err = tr.CurrentUser(ctx)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(owner) == "" {
		return "", "", fmt.Errorf("authenticated claim owner required")
	}
	token, err = lease.LoadClaimToken(root, issue)
	if err != nil {
		return "", "", fmt.Errorf("load claim for issue #%d: %w", issue, err)
	}
	manager.BindClaim(issue, token)
	return owner, token, nil
}

// approvedLeasePolicy loads the lease policy from the task's approved base,
// fetching that commit first when it is not yet local.
func approvedLeasePolicy(root, upstreamRemote, baseSHA string) (*contract.SwarmConfig, time.Duration, error) {
	_ = workspace.FetchCommit(root, upstreamRemote, baseSHA) // best effort; LoadConfigAt fails if the commit is missing
	cfg, err := harness.LoadConfigAt(root, baseSHA)
	if err != nil {
		return nil, 0, err
	}
	duration, err := time.ParseDuration(cfg.Lease.Duration)
	if err != nil {
		return nil, 0, fmt.Errorf("approved lease duration: %w", err)
	}
	return cfg, duration, nil
}

// loadWorkUnit reads an issue and parses its current WorkUnit contract.
func loadWorkUnit(ctx context.Context, tr tracker.Tracker, issue int) (*tracker.Issue, *contract.WorkUnit, error) {
	iss, err := tr.GetIssue(ctx, issue)
	if err != nil {
		return nil, nil, err
	}
	unit, err := contract.ParseWorkUnitFromIssue(iss.Number, iss.Title, iss.Body)
	if err != nil {
		return nil, nil, err
	}
	return iss, unit, nil
}
