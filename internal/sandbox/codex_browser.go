package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// CodexBrowserEligible is shared by diagnostics and launch. BrowserProfile is
// supplied by the coordinator from the approved base, never by task tools.
func CodexBrowserEligible(profile bool, role string, readOnly bool) bool {
	return runtime.GOOS == "linux" && profile && !readOnly && (role == "developer" || role == "tester")
}

// codexBrowserPolicy replaces the entire MCP table. env -i provides an exact
// environment for the actual tool server, independently of Codex's inherited
// MCP environment defaults. No candidate-controlled launcher or config is used.
func codexBrowserPolicy(policy []string, executable, home string, executor ToolExecutor) ([]string, error) {
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(executable) || isSubpath(executable, executor.Work) || isSubpath(executable, home) {
		return nil, fmt.Errorf("codex browser server requires a trusted executable outside task storage")
	}
	args := []string{"-i", "PATH=" + SandboxPath(executor.Dependencies.toolchains()), "HOME=" + home, "TMPDIR=" + home}
	for _, bind := range executor.Dependencies.toolchains() {
		// Chromium discovery must survive the server's private HOME.
		if bind.EnvKey == "PLAYWRIGHT_BROWSERS_PATH" && bind.EnvVal != "" {
			args = append(args, bind.EnvKey+"="+bind.EnvVal)
		}
	}
	// Only host runtime plumbing needed by the verified systemd scope is retained.
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value := os.Getenv(key); value != "" {
			args = append(args, key+"="+value)
		}
	}
	args = append(args, executable)
	args = append(args, internalToolArgs(executor)...)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.Quote(arg)
	}
	setting := `mcp_servers={swarm={command="/usr/bin/env",args=[` + strings.Join(quoted, ",") + `],env_vars=[],required=true,startup_timeout_sec=20,tool_timeout_sec=150,enabled_tools=["swarm_read","swarm_write","swarm_edit","swarm_shell"]}}`
	result := append([]string(nil), policy...)
	for i, arg := range result {
		if arg == "mcp_servers={}" {
			result[i] = setting
			return result, nil
		}
	}
	return nil, fmt.Errorf("codex browser policy requires the default empty MCP table")
}

const codexBrowserGuidance = "Browser checks: use swarm_shell to start the preview and run headless Chromium in the same call. Executor paths are under /workspace. Processes end after each call. Native shell networking remains disabled.\n"
