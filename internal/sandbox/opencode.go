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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// OpenCode owns provider authentication. A private, loopback-only server keeps
// this task separate from the operator's shared service. Its temporary local
// password authenticates SwarmOS to that server, never a model provider.
func runOpenCode(ctx context.Context, binary, model, prompt, control string, env []string, interactive bool, input io.Reader, stdout, stderr io.Writer, guidance ...func(string)) error {
	return runOpenCodeSession(ctx, binary, model, prompt, control, env, false, nil, interactive, input, stdout, stderr, guidance...)
}

// runOpenCodeSession also records telemetry when session is non-nil and the
// inspected `run --help` lists --session. It creates the run's own session up
// front and later fetches only that session's messages, so SwarmOS never lists
// or reads the user's other OpenCode sessions. Any telemetry failure falls back
// to the untracked --continue flow.
func runOpenCodeSession(ctx context.Context, binary, model, prompt, control string, env []string, sessionFlag bool, session *HarnessSession, interactive bool, input io.Reader, stdout, stderr io.Writer, guidance ...func(string)) error {
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
	// Telemetry calls read through a strict cap so a truncated export is never
	// mistaken for a complete one.
	apiTo := func(ctx context.Context, operation string, args ...string) ([]byte, bool) {
		output := &cappedBuffer{limit: telemetryRecordsMax}
		var diagnostics process.Output
		command := append([]string{"api", "--server", started.URL, operation}, args...)
		if runNativeCommand(ctx, binary, control, env, command, nil, output, &diagnostics) != nil || output.overflow {
			return nil, false
		}
		return output.buf, true
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
	var sessionID string
	if session != nil && sessionFlag {
		sessionID = createOpenCodeSession(ctx, apiTo, control)
	}
	if sessionID != "" {
		session.SessionIDs = []string{sessionID}
		args = append(args, "--session", sessionID)
		// Deferred after the server stop, so this runs first, while the server
		// is still up. Its own timeout keeps a hung server from stalling cleanup.
		defer func() {
			fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openCodeTelemetryTimeout)
			defer cancel()
			if raw, ok := apiTo(fetchCtx, "session.messages", "--param", "sessionID="+sessionID); ok {
				session.Records = raw
			}
		}()
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
		if sessionID == "" && !slices.Contains(args, "--continue") {
			args = append(args, "--continue")
		}
		if len(guidance) > 0 && guidance[0] != nil {
			guidance[0](line)
		}
		prompt = "Continue the SwarmOS task.\n" + line
	}
}

const telemetryRecordsMax = 4 << 20

// openCodeTelemetryTimeout bounds each telemetry api call; tests shorten it.
var openCodeTelemetryTimeout = 10 * time.Second

var openCodeSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// createOpenCodeSession returns the id of a new session bound to control, or
// "" when OpenCode cannot create one there.
func createOpenCodeSession(ctx context.Context, api func(context.Context, string, ...string) ([]byte, bool), control string) string {
	ctx, cancel := context.WithTimeout(ctx, openCodeTelemetryTimeout)
	defer cancel()
	raw, ok := api(ctx, "session.create")
	if !ok {
		return ""
	}
	var created struct {
		Data struct {
			ID        string `json:"id"`
			Directory string `json:"directory"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &created) != nil || !openCodeSessionID.MatchString(created.Data.ID) || created.Data.Directory != control {
		return ""
	}
	return created.Data.ID
}

// cappedBuffer keeps at most limit bytes and records whether more arrived. It
// keeps accepting writes so the producer is never blocked.
type cappedBuffer struct {
	limit    int
	buf      []byte
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if len(p) > room {
		b.buf = append(b.buf, p[:max(room, 0)]...)
		b.overflow = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
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
