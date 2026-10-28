package workflow

import (
	"context"
	"fmt"
	"os"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// userCacheDir is replaced by tests so they never touch the real user cache.
var userCacheDir = os.UserCacheDir

// prepareAgentDependencies fills the agents' per-repository dependency
// caches from the approved base on the host. Only native isolated agents
// mount host caches, so other execution modes prepare nothing. Replaced in
// tests.
var prepareAgentDependencies = func(ctx context.Context, execution contract.ExecutionConfig, repo, baseSHA string, policy sandbox.DependencyPolicy) (sandbox.Dependencies, error) {
	if execution.Mode != contract.ExecutionIsolated || execution.Backend != contract.BackendNative {
		return sandbox.Dependencies{Mode: policy.Mode}, nil
	}
	userCache, err := userCacheDir()
	if err != nil {
		userCache = ""
	}
	return sandbox.PrepareDependencyCaches(ctx, sandbox.AgentDependencyCache, repo, baseSHA, userCache, policy)
}

// AgentDependencies prepares the agent dependency caches for baseSHA. It
// never falls back to host caches: an error stops the run before an agent
// starts, and before a claim when called from preflight.
func (e *Engine) AgentDependencies(ctx context.Context, cfg *contract.SwarmConfig, repo, baseSHA string) (sandbox.Dependencies, error) {
	execution, err := contract.ResolveExecution(cfg, e.getenv("SWARM_EXECUTION_MODE"))
	if err != nil {
		return sandbox.Dependencies{}, err
	}
	deps, err := prepareAgentDependencies(ctx, execution, repo, baseSHA, e.DependencyPolicy)
	if err != nil {
		return sandbox.Dependencies{}, fmt.Errorf("dependency cache preflight: %w", err)
	}
	return deps, nil
}
