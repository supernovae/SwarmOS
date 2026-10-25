package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// OpenCode owns provider authentication. A private, loopback-only server keeps
// this task separate from the operator's shared service. Its temporary local
// password authenticates SwarmOS to that server, never a model provider.
func runOpenCode(ctx context.Context, binary, model, prompt, control string, env []string, interactive bool, input io.Reader, stdout, stderr io.Writer, guidance ...func(string)) error {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	env = append(append([]string(nil), env...), "OPENCODE_PASSWORD="+hex.EncodeToString(nonce))
	childCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(childCtx, binary, "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "0")
	cmd.Dir, cmd.Env, cmd.Stderr = control, env, stderr
	process.Configure(cmd)
	cmd.Cancel = func() error { process.Stop(cmd); return nil }
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return err
	}
	defer func() { stop(); process.Stop(cmd); in.Close(); out.Close(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	if !scanner.Scan() {
		return fmt.Errorf("OpenCode private server failed to start")
	}
	var started struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &started); err != nil {
		return fmt.Errorf("invalid OpenCode server handshake: %w", err)
	}
	address, err := url.Parse(started.URL)
	if err != nil || address.Scheme != "http" || address.Hostname() != "127.0.0.1" || address.Port() == "" || address.User != nil || address.RawQuery != "" {
		return fmt.Errorf("OpenCode did not bind a private loopback server")
	}
	// Initialization is asynchronous in V2. Wait for the actual connected catalog,
	// not a fixed startup delay, before submitting the first model request.
	api := func(ctx context.Context, operation string, args ...string) ([]byte, error) {
		var output, diagnostics process.Output
		command := append([]string{"api", "--server", started.URL, operation}, args...)
		if err := runNativeCommand(ctx, binary, control, env, command, nil, &output, &diagnostics); err != nil {
			return nil, fmt.Errorf("OpenCode %s: %w: %s", operation, err, diagnostics.String())
		}
		return []byte(output.String()), nil
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		raw, err := api(readyCtx, "mcp.list")
		if err != nil {
			return err
		}
		var catalog struct {
			Data []struct {
				Name   string `json:"name"`
				Status struct {
					Status string `json:"status"`
				} `json:"status"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &catalog) != nil {
			return fmt.Errorf("OpenCode returned an invalid MCP catalog")
		}
		ready := false
		for _, server := range catalog.Data {
			if server.Name != "swarm" {
				return fmt.Errorf("OpenCode exposed an unapproved MCP server %q", server.Name)
			}
			ready = server.Status.Status == "connected"
		}
		if ready {
			break
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("OpenCode isolated tools are unavailable: %w", readyCtx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	raw, err := api(readyCtx, "agent.get", "--param", "agentID=swarm")
	if err != nil {
		return err
	}
	if err := verifyOpenCodeAgent(raw); err != nil {
		return err
	}
	cancel()
	args := []string{"run", "--server", started.URL, "--agent", "swarm", "--format", "default", "--title", "SwarmOS task"}
	if model != "" {
		args = append(args, "--model", model)
	}
	for {
		turn := append(append([]string(nil), args...), "--", prompt)
		if err := runNativeCommand(ctx, binary, control, env, turn, nil, stdout, stderr); err != nil {
			return err
		}
		if !interactive {
			return nil
		}
		fmt.Fprint(stdout, "\nFollow-up instruction (Enter to finish this phase): ")
		line, err := nativeInstruction(ctx, input)
		if err != nil && err != io.EOF {
			return err
		}
		if strings.TrimSpace(line) == "" {
			return nil
		}
		if !slices.Contains(args, "--continue") {
			args = append(args, "--continue")
		}
		if len(guidance) > 0 && guidance[0] != nil {
			guidance[0](line)
		}
		prompt = "Continue the SwarmOS task.\n" + line
	}
}

// Inspect effective permissions after user/managed settings have been merged.
// A later allow for any host tool must not weaken the generated tool boundary.
func verifyOpenCodeAgent(raw []byte) error {
	var result struct {
		Data struct {
			ID          string                                      `json:"id"`
			Permissions []struct{ Action, Resource, Effect string } `json:"permissions"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Data.ID != "swarm" {
		return fmt.Errorf("OpenCode did not resolve the SwarmOS agent")
	}
	lastDeny := -1
	for i, rule := range result.Data.Permissions {
		if rule.Action == "*" && rule.Resource == "*" && rule.Effect == "deny" {
			lastDeny = i
		}
	}
	if lastDeny < 0 {
		return fmt.Errorf("OpenCode effective permissions have no isolated tool boundary")
	}
	tools := false
	for _, rule := range result.Data.Permissions[lastDeny+1:] {
		if rule.Effect == "deny" {
			continue
		}
		if rule.Action == "swarm_swarm_*" && rule.Resource == "*" && rule.Effect == "allow" {
			tools = true
			continue
		}
		return fmt.Errorf("OpenCode effective permissions enable an unapproved capability %q", rule.Action)
	}
	if !tools {
		return fmt.Errorf("OpenCode effective permissions do not enforce the isolated tool boundary")
	}
	return nil
}
