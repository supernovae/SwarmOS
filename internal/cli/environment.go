package cli

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/node"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workflow"
)

// lookup reads name through the injected lookup, or the process environment
// when none is set.
func (o *RootOptions) lookup(name string) (string, bool) {
	if o.lookupEnv == nil {
		return os.LookupEnv(name)
	}
	return o.lookupEnv(name)
}

func (o *RootOptions) getenv(name string) string {
	value, _ := o.lookup(name)
	return value
}

// userHomeDir mirrors os.UserHomeDir for the injected lookup.
func (o *RootOptions) userHomeDir() (string, bool) {
	name := "HOME"
	switch runtime.GOOS {
	case "windows":
		name = "USERPROFILE"
	case "plan9":
		name = "home"
	}
	home := o.getenv(name)
	return home, home != ""
}

// registryPath is the repository registry file. Without an injected lookup it
// is the registry package default.
func (o *RootOptions) registryPath() string {
	if o.lookupEnv == nil {
		return registry.DefaultRegistryPath()
	}
	if path := o.getenv("SWARM_REGISTRY_PATH"); path != "" {
		return path
	}
	if home, ok := o.userHomeDir(); ok {
		return filepath.Join(home, ".config", "swarm", "repos.yaml")
	}
	return filepath.Join(".", ".swarm", "repos.yaml")
}

// cloneBaseDir is where repo add clones by slug. Without an injected lookup it
// is the registry package default.
func (o *RootOptions) cloneBaseDir() string {
	if o.lookupEnv == nil {
		return registry.DefaultCloneBaseDir()
	}
	if dir := o.getenv("SWARM_REPOS_DIR"); dir != "" {
		return dir
	}
	if home, ok := o.userHomeDir(); ok {
		return filepath.Join(home, ".swarm", "repos")
	}
	return filepath.Join(".", ".swarm", "repos")
}

// nodeConfigPath is the node profile file. Without an injected lookup it is
// the node package default.
func (o *RootOptions) nodeConfigPath() string {
	if o.lookupEnv == nil {
		return node.DefaultNodeConfigPath()
	}
	if path := o.getenv("SWARM_NODE_CONFIG"); path != "" {
		return path
	}
	if home, ok := o.userHomeDir(); ok {
		return filepath.Join(home, ".config", "swarm", "node.yaml")
	}
	return filepath.Join(".", ".swarm", "node.yaml")
}

// localUser mirrors tracker.NewLocalTracker's identity: SWARM_USER, then USER,
// then local-dev.
func (o *RootOptions) localUser() string {
	if user := o.getenv("SWARM_USER"); user != "" {
		return user
	}
	if user := o.getenv("USER"); user != "" {
		return user
	}
	return "local-dev"
}

func (o *RootOptions) runnerOptions() agent.RunnerOptions {
	return agent.RunnerOptions{LookupEnv: o.lookupEnv}
}

func (o *RootOptions) engineOptions() workflow.EngineOptions {
	return workflow.EngineOptions{LookupEnv: o.lookupEnv, NodeProfilePath: o.nodeConfigPath()}
}

// dependencyMode is the node profile's dependency cache mode. A malformed
// profile keeps the default per-repository caches.
func (o *RootOptions) dependencyMode() sandbox.DependencyCacheMode {
	mode, _ := node.DependencyCacheMode(o.nodeConfigPath())
	return mode
}
