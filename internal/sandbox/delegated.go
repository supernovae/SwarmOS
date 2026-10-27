package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// InspectDelegatedHarness checks supported interfaces without opening private
// credential stores. Runtime enforcement is checked separately by the executor.
func InspectDelegatedHarness(ctx context.Context, driver, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	home, err := os.MkdirTemp("", "swarm-harness-inspect-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	env := nativeEnv(home, false, Dependencies{})
	var version, diagnostics process.Output
	if err := runNativeCommand(ctx, binary, home, env, []string{"--version"}, nil, &version, &diagnostics); err != nil {
		return "", fmt.Errorf("%s version check: %w", driver, err)
	}
	var qualified string
	var flags []string
	switch driver {
	case "claude":
		qualified, err = harnessVersion(version.String(), "", " (Claude Code)", [3]int{2, 1, 308})
		flags = []string{"--restricted", "--strict-mcp-config", "--tools", "--settings", "--permission-mode", "--no-session-persistence"}
	case "omp":
		qualified, err = harnessVersion(version.String(), "omp/", "", [3]int{18, 12, 5})
		flags = []string{"--mode", "--no-tools", "--no-extensions", "--no-rules", "--config", "--no-session"}
	case "opencode":
		qualified, err = harnessVersion(version.String(), "opencode v", "", [3]int{2, 1, 2})
		flags = []string{"--standalone", "--server", "--continue", "--model", "--agent", "--format", "--title"}
	default:
		return "", fmt.Errorf("unknown delegated harness %q", driver)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", driver, err)
	}
	var help process.Output
	args := []string{"--help"}
	if driver == "opencode" {
		args = []string{"run", "--help"}
	}
	if err := runNativeCommand(ctx, binary, home, env, args, nil, &help, &diagnostics); err != nil {
		return "", fmt.Errorf("%s interface inspection: %w", driver, err)
	}
	for _, flag := range flags {
		if !strings.Contains(help.String(), flag) {
			return "", fmt.Errorf("%s %s lacks required interface %s", driver, qualified, flag)
		}
	}
	return qualified, nil
}

func ClaudeAuthentication(ctx context.Context, binary string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var output, diagnostics process.Output
	err = runNativeCommand(ctx, binary, os.TempDir(), nativeEnv(home, true, Dependencies{}), []string{"auth", "status"}, nil, &output, &diagnostics)
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err != nil || json.Unmarshal([]byte(output.String()), &status) != nil || !status.LoggedIn {
		return "", fmt.Errorf("claude login is unavailable; run claude auth login, then swarm repo doctor")
	}
	return "Claude-managed " + status.AuthMethod + " login", nil
}

func writeHarnessJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// The host-side harness starts in a coordinator-owned control directory, never
// in candidate code. Its only execution surface is the selected tool transport.
func RunDelegatedHarness(ctx context.Context, dir, driver, binary, model, prompt string, opts WorkerOptions, interactive bool, input io.Reader, stdout, stderr io.Writer) ([]string, string, error) {
	preflight := time.Now()
	version, err := opts.Inspections.InspectDelegatedHarness(ctx, driver, binary, opts.InspectionPolicy)
	if err != nil {
		return nil, "", err
	}
	if err := CheckToolExecutor(ctx); err != nil {
		return nil, "", err
	}
	if driver == "claude" {
		if _, err := ClaudeAuthentication(ctx, binary); err != nil {
			return nil, "", err
		}
	}
	opts.Lifecycle.addPreflight(preflight)
	var command []string
	err = withNativeWorkspace(ctx, dir, opts, func(ctx context.Context, work, scratch string) error {
		control := filepath.Join(filepath.Dir(scratch), "control")
		if err := os.Mkdir(control, 0700); err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		env := nativeEnv(home, true, opts.Dependencies)
		// dir is the real worktree, so the cache key names its repository.
		caches, err := agentBuildCaches(ctx, dir, scratch, opts)
		if err != nil {
			return err
		}
		executor := ToolExecutor{Work: work, ReadOnly: opts.ReadOnly, BuildCaches: caches, Dependencies: opts.Dependencies}
		prompt = "The task files are in /workspace inside the SwarmOS tools. Use swarm_read, swarm_write, swarm_edit and swarm_shell for file access, editing, searching and testing.\n" + prompt
		if driver == "omp" {
			command = []string{binary, "--mode", "rpc"}
			return runOMP(ctx, binary, model, prompt, control, env, executor, interactive, input, stdout, stderr, opts.GuidanceDelivered)
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return err
		}
		toolArgs := internalToolArgs(executor)
		var args []string
		switch driver {
		case "claude":
			config := filepath.Join(control, "mcp.json")
			if err := writeHarnessJSON(config, map[string]any{"mcpServers": map[string]any{"swarm": map[string]any{"command": executable, "args": toolArgs}}}); err != nil {
				return err
			}
			settings := filepath.Join(control, "settings.json")
			if err := writeHarnessJSON(settings, map[string]any{"disableAllHooks": true, "enabledPlugins": map[string]any{}, "permissions": map[string]any{"allow": []string{"mcp__swarm__*"}}}); err != nil {
				return err
			}
			args = []string{"--restricted", "--strict-mcp-config", "--mcp-config", config, "--tools", "", "--disable-slash-commands", "--no-chrome", "--settings", settings, "--permission-mode", "dontAsk", "--permission-prompts", "none", "--no-session-persistence", "--print"}
		case "opencode":
			env, err = prepareOpenCode(control, executable, toolArgs, env)
			if err != nil {
				return err
			}
			command = []string{binary, "serve", "--stdio"}
			return runOpenCode(ctx, binary, model, prompt, control, env, interactive, input, stdout, stderr, opts.GuidanceDelivered)
		default:
			return fmt.Errorf("unsupported harness %q", driver)
		}
		if model != "" {
			args = append(args, "--model", model)
		}
		return runFollowupTurns(ctx, prompt, "Continue the task using the SwarmOS tools. Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md.\n", interactive, false, input, stdout, func(prompt string) error {
			turn := append(append([]string(nil), args...), "--", prompt)
			command = append([]string{binary}, turn...)
			return runNativeCommand(ctx, binary, control, env, turn, nil, stdout, stderr)
		}, opts.GuidanceDelivered)
	})
	return command, version, err
}

// internalToolArgs starts the separate `swarm internal-tools` process. It sees
// only the detached snapshot, so the agent caches travel as flags.
func internalToolArgs(executor ToolExecutor) []string {
	args := []string{"internal-tools", "--workspace", executor.Work}
	if executor.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, c := range executor.BuildCaches {
		args = append(args, "--build-cache", buildCacheFlag(c))
	}
	if executor.Dependencies.Mode == SharedHostCaches {
		args = append(args, "--share-host-dependency-caches")
	}
	for _, c := range executor.Dependencies.Caches {
		args = append(args, "--dependency-cache", dependencyCacheFlag(c))
	}
	return args
}

func prepareOpenCode(control, executable string, toolArgs, env []string) ([]string, error) {
	// V2 uses policies/permissions arrays and mcp.servers. Keep provider-owned
	// data at its normal XDG location; only customization discovery is redirected.
	config := filepath.Join(control, "config", "opencode")
	if err := os.MkdirAll(config, 0700); err != nil {
		return nil, err
	}
	permissions := []map[string]string{{"action": "*", "resource": "*", "effect": "deny"}, {"action": "swarm_swarm_*", "resource": "*", "effect": "allow"}}
	settings := map[string]any{
		"permissions":  permissions,
		"agents":       map[string]any{"swarm": map[string]any{"mode": "primary", "description": "SwarmOS task worker", "permissions": permissions}},
		"policies":     []map[string]string{{"action": "tool.use", "resource": "*", "effect": "deny"}, {"action": "tool.use", "resource": "swarm_swarm_*:*", "effect": "allow"}, {"action": "integration.use", "resource": "mcp:*", "effect": "deny"}, {"action": "integration.use", "resource": "mcp:swarm", "effect": "allow"}, {"action": "integration.use", "resource": "skill:*", "effect": "deny"}},
		"mcp":          map[string]any{"servers": map[string]any{"swarm": map[string]any{"type": "local", "command": append([]string{executable}, toolArgs...), "codemode": false}}},
		"instructions": []string{}, "references": map[string]any{},
	}
	if err := writeHarnessJSON(filepath.Join(config, "opencode.json"), settings); err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
			filtered = append(filtered, entry)
		}
	}
	filtered = append(filtered, "XDG_CONFIG_HOME="+filepath.Dir(config))
	return filtered, nil
}
