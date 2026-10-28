package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/supernovae/swarmos/internal/process"
)

// RPC host tools are an OMP-supported execution interface. Authentication and
// model transport remain entirely within OMP, using its existing login.
func runOMP(ctx context.Context, binary, model, prompt, control string, env []string, executor ToolExecutor, interactive bool, input io.Reader, stdout, stderr io.Writer, guidance ...func(string)) error {
	config := filepath.Join(control, "omp.json")
	settings := map[string]any{
		"disabledProviders": []string{"native", "claude", "codex", "gemini", "github", "opencode", "cursor", "agents-md"},
		"extensions":        []string{}, "contextFiles": map[string]any{"extra": []string{}},
		"mcp":   map[string]any{"enableProjectConfig": false},
		"async": map[string]any{"enabled": false}, "memory": map[string]any{"backend": "off"},
		"advisor": map[string]any{"enabled": false}, "startup": map[string]any{"checkUpdate": false, "setupWizard": false},
		"tools": map[string]any{"xdev": false, "approvalMode": "write", "approval": map[string]string{"swarm_read": "allow", "swarm_write": "allow", "swarm_edit": "allow", "swarm_shell": "allow"}},
	}
	if err := writeHarnessJSON(config, settings); err != nil {
		return err
	}
	args := []string{"--mode", "rpc", "--no-ui", "--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "--config", config}
	if model != "" {
		args = append(args, "--model", model)
	}
	childCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(childCtx, binary, args...)
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
	var writeMu sync.Mutex
	send := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return json.NewEncoder(in).Encode(value)
	}
	var pendingMu sync.Mutex
	pending := map[string]context.CancelFunc{}
	var workers sync.WaitGroup
	defer func() { stop(); workers.Wait() }()
	tools := make([]map[string]any, 0, 4)
	for _, tool := range executorTools() {
		tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema, "loadMode": "essential"})
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	ready, configured, completed := false, false, false
	turn := 1
	for scanner.Scan() {
		var frame struct {
			Type                  string          `json:"type"`
			ID                    string          `json:"id"`
			Command               string          `json:"command"`
			Success               bool            `json:"success"`
			Status                string          `json:"status"`
			SessionSettled        bool            `json:"sessionSettled"`
			AgentInvoked          bool            `json:"agentInvoked"`
			ToolName              string          `json:"toolName"`
			Arguments             json.RawMessage `json:"arguments"`
			TargetID              string          `json:"targetId"`
			Data                  json.RawMessage `json:"data"`
			Error                 json.RawMessage `json:"error"`
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return fmt.Errorf("invalid OMP RPC frame: %w", err)
		}
		switch frame.Type {
		case "ready":
			if ready {
				return fmt.Errorf("duplicate OMP ready frame")
			}
			ready = true
			if err := send(map[string]any{"id": "swarm-tools", "type": "set_host_tools", "tools": tools}); err != nil {
				return err
			}
		case "response":
			if !frame.Success {
				return fmt.Errorf("OMP %s failed: %s", frame.Command, frame.Error)
			}
			if frame.ID == "swarm-tools" {
				var data struct {
					ToolNames []string `json:"toolNames"`
				}
				if json.Unmarshal(frame.Data, &data) != nil || len(data.ToolNames) != len(tools) {
					return fmt.Errorf("OMP did not register the isolated tools")
				}
				registered := map[string]bool{}
				for _, name := range data.ToolNames {
					registered[name] = true
				}
				for _, tool := range executorTools() {
					if !registered[tool.Name] {
						return fmt.Errorf("OMP returned unexpected tool registration")
					}
				}
				if err := send(map[string]any{"id": "swarm-inspect", "type": "get_state"}); err != nil {
					return err
				}
			}
			if frame.ID == "swarm-inspect" {
				var state struct {
					DumpTools []struct {
						Name string `json:"name"`
					} `json:"dumpTools"`
				}
				if json.Unmarshal(frame.Data, &state) != nil || len(state.DumpTools) != len(tools) {
					return fmt.Errorf("OMP exposed an unexpected tool surface")
				}
				allowed := map[string]bool{}
				for _, tool := range executorTools() {
					allowed[tool.Name] = true
				}
				for _, tool := range state.DumpTools {
					if !allowed[tool.Name] {
						return fmt.Errorf("OMP exposed unmediated tool %q", tool.Name)
					}
					delete(allowed, tool.Name)
				}
				if len(allowed) != 0 {
					return fmt.Errorf("OMP omitted isolated tools")
				}
				configured = true
				if prompt == "" {
					return nil
				} // Interface qualification without a model request.
				if err := send(map[string]any{"id": fmt.Sprintf("turn-%d", turn), "type": "prompt", "message": prompt}); err != nil {
					return err
				}
			}
		case "host_tool_call":
			if !configured || frame.ID == "" {
				return fmt.Errorf("unexpected OMP tool request")
			}
			pendingMu.Lock()
			if _, exists := pending[frame.ID]; exists || len(pending) >= 8 {
				pendingMu.Unlock()
				return fmt.Errorf("OMP exceeded tool concurrency limit")
			}
			callCtx, cancel := context.WithCancel(childCtx)
			pending[frame.ID] = cancel
			pendingMu.Unlock()
			workers.Add(1)
			go func(id, name string, arguments json.RawMessage) {
				defer workers.Done()
				defer cancel()
				result := executor.Call(callCtx, name, arguments)
				_ = send(map[string]any{"type": "host_tool_result", "id": id, "result": result, "isError": result.IsError})
				pendingMu.Lock()
				delete(pending, id)
				pendingMu.Unlock()
			}(frame.ID, frame.ToolName, frame.Arguments)
		case "host_tool_cancel":
			pendingMu.Lock()
			if cancel := pending[frame.TargetID]; cancel != nil {
				cancel()
			}
			pendingMu.Unlock()
		case "message_update":
			if frame.AssistantMessageEvent.Type == "text_delta" {
				fmt.Fprint(stdout, frame.AssistantMessageEvent.Delta)
			}
		case "prompt_result":
			if frame.ID != fmt.Sprintf("turn-%d", turn) || !frame.AgentInvoked || frame.Status != "completed" {
				return fmt.Errorf("OMP task did not complete: %s %s", frame.Status, frame.Error)
			}
			completed = true
			if !frame.SessionSettled {
				continue
			}
		case "session_settled":
			if !completed {
				continue
			}
		case "rpc_frame_error", "extension_error", "host_uri_request":
			return fmt.Errorf("unexpected OMP capability or protocol error: %s", frame.Type)
		default:
			continue
		}
		if completed && (frame.Type == "session_settled" || frame.Type == "prompt_result" && frame.SessionSettled) {
			workers.Wait()
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
			turn++
			completed = false
			if len(guidance) > 0 && guidance[0] != nil {
				guidance[0](line)
			}
			prompt = "Continue the SwarmOS task.\n" + line
			if err := send(map[string]any{"id": "swarm-inspect", "type": "get_state"}); err != nil {
				return err
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("OMP closed its RPC stream before the task completed")
}
