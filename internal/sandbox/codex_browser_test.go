package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/supernovae/swarmos/internal/process"
)

func TestCodexBrowserPolicy(t *testing.T) {
	executable, _ := os.Executable()
	base := codexPolicy(executable, "/task", "/scratch", false, nil, Dependencies{})
	policy, err := codexBrowserPolicy(base, executable, t.TempDir(), ToolExecutor{BrowserProfile: true, Work: "/task"})
	if err != nil {
		t.Fatal(err)
	}
	// Decode the complete generated configuration so extra servers or approval
	// grants cannot hide behind a matching substring.
	var config struct {
		ApprovalPolicy string `toml:"approval_policy"`
		MCPServers     map[string]struct {
			EnabledTools    []string `toml:"enabled_tools"`
			DefaultApproval string   `toml:"default_tools_approval_mode"`
			Tools           map[string]struct {
				ApprovalMode string `toml:"approval_mode"`
			} `toml:"tools"`
		} `toml:"mcp_servers"`
	}
	var settings []string
	for i, arg := range policy {
		if i > 0 && policy[i-1] == "-c" {
			settings = append(settings, arg)
		}
	}
	if _, err := toml.Decode(strings.Join(settings, "\n"), &config); err != nil {
		t.Fatal(err)
	}
	if config.ApprovalPolicy != "never" || len(config.MCPServers) != 1 {
		t.Fatalf("unexpected approval policy or servers: %+v", config)
	}
	server, ok := config.MCPServers["swarm"]
	wantTools := []string{"swarm_read", "swarm_write", "swarm_edit", "swarm_shell"}
	if !ok || server.DefaultApproval != "" || !reflect.DeepEqual(server.EnabledTools, wantTools) || len(server.Tools) != len(wantTools) {
		t.Fatalf("unexpected server approvals: %+v", config.MCPServers)
	}
	for _, name := range wantTools {
		if server.Tools[name].ApprovalMode != "approve" {
			t.Fatalf("tool %s is not pre-approved: %+v", name, server.Tools)
		}
	}
	changes := 0
	for i := range base {
		if base[i] != policy[i] {
			changes++
			if base[i] != "mcp_servers={}" {
				t.Fatalf("native permissions changed: %s", base[i])
			}
			for _, want := range []string{`command="/usr/bin/env"`, `"-i"`, `env_vars=[]`, `required=true`, `"--browser-profile"`, `enabled_tools=["swarm_read","swarm_write","swarm_edit","swarm_shell"]`} {
				if !strings.Contains(policy[i], want) {
					t.Fatalf("missing %s", want)
				}
			}
		}
	}
	if changes != 1 || !reflect.DeepEqual(base, codexPolicy(executable, "/task", "/scratch", false, nil, Dependencies{})) {
		t.Fatal("default policy changed")
	}
	for _, role := range []string{"developer", "tester", "planner", "reviewer", ""} {
		for _, profile := range []bool{false, true} {
			for _, ro := range []bool{false, true} {
				want := runtime.GOOS == "linux" && profile && !ro && (role == "developer" || role == "tester")
				if CodexBrowserEligible(profile, role, ro) != want {
					t.Fatal("eligibility", profile, role, ro)
				}
			}
		}
	}
	if _, err := codexBrowserPolicy(base, executable, filepath.Dir(executable), ToolExecutor{Work: "/task"}); err == nil {
		t.Fatal("writable launcher accepted")
	}
}

// checkCodexBrowserCatalog accepts the owned namespace and Codex's native tools.
// An explicit built-in list keeps an unexpected flat MCP tool from passing.
func checkCodexBrowserCatalog(raw json.RawMessage) error {
	var tools []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return err
	}
	owned := 0
	for _, tool := range tools {
		if tool.Type == "namespace" {
			if tool.Name != "mcp__swarm" {
				return fmt.Errorf("unexpected namespace %q", tool.Name)
			}
			owned++
			names := map[string]bool{"swarm_read": false, "swarm_write": false, "swarm_edit": false, "swarm_shell": false}
			if len(tool.Tools) != len(names) {
				return fmt.Errorf("owned namespace has %d tools", len(tool.Tools))
			}
			for _, child := range tool.Tools {
				seen, ok := names[child.Name]
				if child.Type != "function" || !ok || seen {
					return fmt.Errorf("unexpected or duplicate owned tool %q (%s)", child.Name, child.Type)
				}
				names[child.Name] = true
			}
			continue
		}
		if len(tool.Tools) != 0 {
			return fmt.Errorf("unexpected nested tools on %q", tool.Name)
		}
		switch tool.Type {
		case "function", "custom":
			switch tool.Name {
			case "exec_command", "write_stdin", "view_image", "request_user_input", "list_mcp_resources", "list_mcp_resource_templates", "read_mcp_resource":
			default:
				return fmt.Errorf("unexpected top-level tool %q", tool.Name)
			}
		default:
			return fmt.Errorf("unexpected tool type %q", tool.Type)
		}
	}
	if owned != 1 {
		return fmt.Errorf("expected one mcp__swarm namespace, got %d", owned)
	}
	return nil
}

func TestCodexBrowserProviderCatalog(t *testing.T) {
	owned := `{"type":"namespace","name":"mcp__swarm","tools":[{"type":"function","name":"swarm_read"},{"type":"function","name":"swarm_write"},{"type":"function","name":"swarm_edit"},{"type":"function","name":"swarm_shell"}]}`
	for _, tc := range []struct {
		name, catalog string
		valid         bool
	}{
		{"owned and native", `[` + owned + `,{"type":"function","name":"exec_command"},{"type":"function","name":"write_stdin"},{"type":"function","name":"view_image"},{"type":"function","name":"request_user_input"},{"type":"function","name":"list_mcp_resources"},{"type":"function","name":"list_mcp_resource_templates"},{"type":"function","name":"read_mcp_resource"}]`, true},
		{"reject shell function", `[` + owned + `,{"type":"function","name":"shell"}]`, false},
		{"reject local_shell function", `[` + owned + `,{"type":"function","name":"local_shell"}]`, false},
		{"reject apply_patch function", `[` + owned + `,{"type":"function","name":"apply_patch"}]`, false},
		{"reject update_plan function", `[` + owned + `,{"type":"function","name":"update_plan"}]`, false},
		{"reject web_search function", `[` + owned + `,{"type":"function","name":"web_search"}]`, false},
		{"reject web_search_preview function", `[` + owned + `,{"type":"function","name":"web_search_preview"}]`, false},
		{"reject image_generation function", `[` + owned + `,{"type":"function","name":"image_generation"}]`, false},
		{"reject local_shell type", `[` + owned + `,{"type":"local_shell"}]`, false},
		{"reject web_search type", `[` + owned + `,{"type":"web_search"}]`, false},
		{"reject web_search_preview type", `[` + owned + `,{"type":"web_search_preview"}]`, false},
		{"reject image_generation type", `[` + owned + `,{"type":"image_generation"}]`, false},
		{"missing", `[]`, false},
		{"duplicate namespace", `[` + owned + `,` + owned + `]`, false},
		{"foreign namespace", `[` + owned + `,{"type":"namespace","name":"mcp__host","tools":[]}]`, false},
		{"flat MCP", `[` + owned + `,{"type":"function","name":"mcp__swarm__swarm_shell"}]`, false},
		{"unknown flat tool", `[` + owned + `,{"type":"function","name":"host_shell"}]`, false},
		{"duplicate child", `[` + strings.Replace(owned, "swarm_read", "swarm_shell", 1) + `]`, false},
		{"missing child", `[` + strings.Replace(owned, `,{"type":"function","name":"swarm_shell"}`, "", 1) + `]`, false},
		{"foreign child", `[` + strings.Replace(owned, "swarm_read", "host_shell", 1) + `]`, false},
		{"nonfunction child", `[` + strings.Replace(owned, `"type":"function"`, `"type":"custom"`, 1) + `]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkCodexBrowserCatalog(json.RawMessage(tc.catalog)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

// Real Codex talks to a local Responses provider; no login or paid model is used.
// The catalog observed by the provider proves initialize/list, and probe.txt
// proves the actual advertised tool was called through the installed CLI.
func TestCodexBrowserRealTools(t *testing.T) {
	binary := os.Getenv("SWARM_TEST_CODEX_BINARY")
	if binary == "" || os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("requires SWARM_TEST_CODEX_BINARY and SWARM_TEST_EXECUTOR=1 on a socket-capable Linux executor host")
	}
	for _, behavior := range []string{"developer", "tester", "forged-host-tool", "cancellation"} {
		t.Run(behavior, func(t *testing.T) {
			requireLoopbackCanary(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := CheckBrowserProfile(ctx); err != nil {
				t.Fatal(err)
			}
			work, home, control := t.TempDir(), t.TempDir(), t.TempDir()
			executable, _ := os.Executable()
			// Test-only trusted launcher captures the actual server environment before
			// exec. It lives outside the native filesystem profile, as does its log.
			launcher := filepath.Join(control, "server")
			serverLog := filepath.Join(control, "server-stderr")
			script := "#!/bin/sh\nexec 2> " + strconv.Quote(serverLog) + "\n/usr/bin/env > " + strconv.Quote(filepath.Join(control, "environment")) + "\necho $$ > " + strconv.Quote(filepath.Join(control, "pid")) + "\nexec " + strconv.Quote(executable) + " \"$@\"\n"
			if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var catalog atomic.Bool
			var providerTools process.Output
			var out, stderr process.Output
			var runErr error
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("Codex result: %v; provider requests: %d\nCodex stdout (tail):\n%s\nCodex stderr (tail):\n%s\nMCP server stderr (tail):\n%s\nProvider tool catalogs (tail):\n%s",
						runErr, calls.Load(), codexBrowserLogTail(out.String()), codexBrowserLogTail(stderr.String()), codexBrowserFileTail(serverLog), codexBrowserLogTail(providerTools.String()))
				}
			})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					// Keep the full wire catalog to diagnose namespaces/deferred tools
					// as well as failures to start or list the MCP server.
					Tools json.RawMessage `json:"tools"`
					Input json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				fmt.Fprintf(&providerTools, "%s\n", request.Tools)
				if err := checkCodexBrowserCatalog(request.Tools); err != nil {
					t.Error(err)
					http.Error(w, "invalid tool catalog", 400)
					return
				}
				catalog.Store(true)
				n := calls.Add(1)
				if behavior == "forged-host-tool" && n == 2 {
					var input []struct {
						Type   string          `json:"type"`
						CallID string          `json:"call_id"`
						Output json.RawMessage `json:"output"`
					}
					if err := json.Unmarshal(request.Input, &input); err != nil {
						t.Error(err)
					}
					refused := false
					for _, item := range input {
						if item.Type == "function_call_output" && item.CallID == "call_1" && strings.Contains(strings.ToLower(string(item.Output)), "unsupported") {
							refused = true
						}
					}
					if !refused {
						t.Errorf("forged host call did not return an unsupported-call refusal: %s", request.Input)
					}
				}
				output := []any{}
				if n == 1 {
					namespace, name, command := "mcp__swarm", "swarm_shell", "env > tool-env; printf allowed > probe.txt"
					if behavior == "developer" || behavior == "tester" {
						fixture, err := os.ReadFile("testdata/browser-preview.sh")
						if err != nil {
							t.Error(err)
							http.Error(w, "fixture missing", 500)
							return
						}
						command = "env > tool-env; printf allowed > probe.txt;\n" + string(fixture)
					}
					if behavior == "forged-host-tool" {
						namespace, name = "mcp__host", "host_shell"
						command = "printf escaped > forged"
					}
					if behavior == "cancellation" {
						command = "echo started > started; setsid sh -c 'sleep 2; echo escaped > survived' & wait"
					}
					args, _ := json.Marshal(map[string]string{"command": command})
					output = append(output, map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "namespace": namespace, "name": name, "arguments": string(args), "status": "completed"})
				} else {
					output = append(output, map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": "done"}}})
				}
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(kind string, value any) {
					data, _ := json.Marshal(value)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
				}
				emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress", "output": []any{}}})
				for i, item := range output {
					emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
					emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				}
				emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "status": "completed", "output": output, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
			}))
			defer provider.Close()
			args, err := codexBrowserPolicy(codexPolicy(binary, work, home, false, nil, Dependencies{}), launcher, home, ToolExecutor{BrowserProfile: true, Work: work})
			if err != nil {
				t.Fatal(err)
			}
			args = append(args, "-c", `model_provider="fixture"`, "-c", `model_providers={fixture={name="fixture",base_url=`+strconv.Quote(provider.URL+"/v1")+`,wire_api="responses",requires_openai_auth=false,request_max_retries=0,stream_max_retries=0}}`)
			args = append(args, codexExecArgs(work, "fixture", "")...)
			args = append(args, "Run the fixture tool once.")
			started := make(chan bool, 1)
			if behavior == "cancellation" {
				go func() {
					for {
						if _, err := os.Stat(filepath.Join(work, "started")); err == nil {
							started <- true
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							started <- false
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
				}()
			}
			env := append(nativeEnv(home, false, Dependencies{}), "RUST_LOG=codex_rmcp_client=debug,codex_core=info", "GH_TOKEN=synthetic-coordinator", "OPENAI_API_KEY=synthetic-provider", "SWARM_GITHUB_TOKEN_COMMAND=synthetic-command")
			runErr = runNativeCommand(ctx, binary, work, env, args, nil, &out, &stderr)
			// Codex and its MCP child must both finish before the launcher returns.
			pidBytes, pidErr := os.ReadFile(filepath.Join(control, "pid"))
			if pidErr != nil {
				t.Fatal("missing child pid", pidErr)
			}
			pid, pidErr := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if pidErr != nil {
				t.Fatal(pidErr)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				_, statErr := os.Stat(fmt.Sprintf("/proc/%d", pid))
				if os.IsNotExist(statErr) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("MCP child survived Codex teardown")
				}
				time.Sleep(10 * time.Millisecond)
			}
			captured, err := os.ReadFile(filepath.Join(control, "environment"))
			if err != nil {
				t.Fatal("server never started", err)
			}
			for _, key := range []string{"GH_TOKEN=", "OPENAI_API_KEY=", "SWARM_GITHUB_TOKEN_COMMAND=", "SSH_AUTH_SOCK="} {
				if strings.Contains(string(captured), key) {
					t.Fatalf("server inherited credential %s", key)
				}
			}
			if !catalog.Load() {
				t.Fatal("initialize/list did not expose four owned tools")
			}
			if behavior == "cancellation" {
				if !<-started || runErr == nil {
					t.Fatal("tool cancellation not exercised")
				}
				time.Sleep(2200 * time.Millisecond)
				if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
					t.Fatal("tool descendant survived")
				}
				return
			}
			if behavior == "forged-host-tool" {
				if calls.Load() < 2 {
					t.Fatal("forged call refusal not returned to provider")
				}
				if _, err := os.Stat(filepath.Join(work, "forged")); !os.IsNotExist(err) {
					t.Fatal("forged tool executed")
				}
				return
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			data, err := os.ReadFile(filepath.Join(work, "probe.txt"))
			if err != nil || string(data) != "allowed" || calls.Load() < 2 {
				t.Fatal("tool cycle failed", err)
			}
			for _, name := range []string{"dom.html", "preview.png"} {
				artifact, err := os.ReadFile(filepath.Join(work, "browser-output", name))
				if err != nil {
					t.Fatal(err)
				}
				if name == "dom.html" && !bytes.Contains(artifact, []byte(`<p id="result">SWARM_BROWSER_OK</p>`)) {
					t.Fatal("DOM marker missing")
				}
				if name == "preview.png" && !bytes.HasPrefix(artifact, []byte("\x89PNG\r\n\x1a\n")) {
					t.Fatal("invalid PNG")
				}
			}
			data, err = os.ReadFile(filepath.Join(work, "tool-env"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "synthetic-") {
				t.Fatal("payload inherited credentials")
			}
		})
	}
}

func TestCodexBrowserTransport(t *testing.T) {
	// Exercise the exact server protocol without a socket or executor prerequisite.
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"resources/list"}
`)
	var output bytes.Buffer
	if err := ServeTools(context.Background(), ToolExecutor{}, input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for i := 1; i <= 3; i++ {
		var frame map[string]json.RawMessage
		if err := decoder.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 1:
			if !bytes.Contains(frame["result"], []byte("swarm-tools")) {
				t.Fatal("initialize failed")
			}
		case 2:
			var catalog struct {
				Tools []ToolDefinition `json:"tools"`
			}
			if err := json.Unmarshal(frame["result"], &catalog); err != nil {
				t.Fatal(err)
			}
			expected, _ := json.Marshal(executorTools())
			actual, _ := json.Marshal(catalog.Tools)
			if !bytes.Equal(actual, expected) {
				t.Fatal("unexpected catalog")
			}
		case 3:
			if len(frame["error"]) == 0 {
				t.Fatal("resources exposed")
			}
		}
	}
	for _, tc := range []struct{ name, args string }{
		{"host_shell", `{"command":"true"}`},
		{"swarm_shell", `{"command":"true","browser_profile":"false"}`},
		{"swarm_shell", `{"command":1}`},
		{"swarm_shell", `{}`},
		{"swarm_shell", strings.Repeat("x", 129<<10)},
	} {
		if !(ToolExecutor{}).Call(context.Background(), tc.name, json.RawMessage(tc.args)).IsError {
			t.Fatal("unauthorized call accepted", tc.name)
		}
	}
}

const codexBrowserDiagnosticLimit = 16 << 10

func codexBrowserLogTail(text string) string {
	if len(text) > codexBrowserDiagnosticLimit {
		return "[earlier output truncated]\n" + text[len(text)-codexBrowserDiagnosticLimit:]
	}
	return text
}

func codexBrowserFileTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("[stderr unavailable: %v]", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Sprintf("[stderr stat failed: %v]", err)
	}
	// Read at most the diagnostic budget plus one byte to retain truncation
	// information, even when the server wrote a very large stderr file.
	if info.Size() > codexBrowserDiagnosticLimit {
		if _, err := file.Seek(info.Size()-codexBrowserDiagnosticLimit-1, io.SeekStart); err != nil {
			return fmt.Sprintf("[stderr seek failed: %v]", err)
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, codexBrowserDiagnosticLimit+1))
	if err != nil {
		return fmt.Sprintf("[stderr read failed: %v]", err)
	}
	return codexBrowserLogTail(string(data))
}

func TestCodexBrowserDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr")
	if got := codexBrowserFileTail(path); !strings.Contains(got, "stderr unavailable") {
		t.Fatal("missing stderr was not diagnosed", got)
	}
	for _, data := range []string{"", "server exited early\n", strings.Repeat("x", 2*codexBrowserDiagnosticLimit) + "last error\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if got := codexBrowserFileTail(path); got != codexBrowserLogTail(data) {
			t.Fatalf("file diagnostics differ from bounded process diagnostics: %d bytes", len(got))
		}
	}
}

func TestCodexBrowserServerChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux transport")
	}
	binary, _ := os.Executable()
	home, work, control := t.TempDir(), t.TempDir(), t.TempDir()
	launcher := filepath.Join(control, "server")
	script := "#!/bin/sh\n/usr/bin/env > " + strconv.Quote(filepath.Join(control, "environment")) + "\nexec " + strconv.Quote(binary) + " \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	policy, err := codexBrowserPolicy([]string{"-c", "mcp_servers={}"}, launcher, home, ToolExecutor{Work: work, BrowserProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Command      string
			Args         []string
			EnabledTools []string `toml:"enabled_tools"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(policy[1], &config); err != nil {
		t.Fatal(err)
	}
	server, ok := config.Servers["swarm"]
	if !ok || len(config.Servers) != 1 {
		t.Fatal("exclusive server missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, server.Command, server.Args...)
	child.Env = []string{"GH_TOKEN=synthetic-coordinator", "OPENAI_API_KEY=synthetic-provider", "RUST_LOG=codex_rmcp_client=debug,codex_core=info"}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	child.Stderr = &diagnostics
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = child.Wait() }()
	encoder, decoder := json.NewEncoder(input), json.NewDecoder(output)
	for i, request := range []map[string]any{
		{"method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "codex-probe", "version": "0.166.0"}}},
		{"method": "tools/list"},
		{"method": "tools/call", "params": map[string]any{"name": "host_shell", "arguments": map[string]string{"command": "true"}}},
	} {
		request["jsonrpc"] = "2.0"
		request["id"] = i + 1
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result json.RawMessage `json:"result"`
		}
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err, diagnostics.String())
		}
		if i == 0 {
			var initialized struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(response.Result, &initialized); err != nil || initialized.ProtocolVersion != "2024-11-05" {
				t.Fatal("protocol negotiation failed", err, string(response.Result))
			}
			if err := encoder.Encode(map[string]string{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
				t.Fatal(err)
			}
		}
		if i == 1 {
			var catalog struct {
				Tools []ToolDefinition `json:"tools"`
			}
			if err := json.Unmarshal(response.Result, &catalog); err != nil {
				t.Fatal(err)
			}
			names := make([]string, len(catalog.Tools))
			for j, tool := range catalog.Tools {
				names[j] = tool.Name
			}
			if !reflect.DeepEqual(names, server.EnabledTools) {
				t.Fatalf("enabled_tools %v differ from advertised names %v", server.EnabledTools, names)
			}
		}
		if i == 2 {
			var result ToolResult
			if err := json.Unmarshal(response.Result, &result); err != nil || !result.IsError {
				t.Fatal("forged tool accepted", err)
			}
		}
	}
	captured, err := os.ReadFile(filepath.Join(control, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GH_TOKEN=", "OPENAI_API_KEY=", "SWARM_GITHUB_TOKEN_COMMAND=", "SSH_AUTH_SOCK=", "RUST_LOG=", "SWARM_TEST_EXECUTOR="} {
		if strings.Contains(string(captured), key) {
			t.Fatal("credential in actual child", key)
		}
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err, diagnostics.String())
	}
}
