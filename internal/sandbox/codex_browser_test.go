package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/BurntSushi/toml"
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

	"github.com/supernovae/swarmos/internal/process"
)

func TestCodexBrowserPolicy(t *testing.T) {
	executable, _ := os.Executable()
	base := codexPolicy(executable, "/task", "/scratch", false, nil, Dependencies{})
	policy, err := codexBrowserPolicy(base, executable, t.TempDir(), ToolExecutor{BrowserProfile: true, Work: "/task"})
	if err != nil {
		t.Fatal(err)
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
			script := "#!/bin/sh\n/usr/bin/env > " + strconv.Quote(filepath.Join(control, "environment")) + "\necho $$ > " + strconv.Quote(filepath.Join(control, "pid")) + "\nexec " + strconv.Quote(executable) + " \"$@\"\n"
			if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var catalog atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					Input json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&request); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				owned := 0
				for _, tool := range request.Tools {
					if strings.HasPrefix(tool.Name, "mcp__swarm__") {
						owned++
						if !strings.Contains("|swarm_read|swarm_write|swarm_edit|swarm_shell|", "|"+strings.TrimPrefix(tool.Name, "mcp__swarm__")+"|") {
							t.Errorf("unexpected owned tool %s", tool.Name)
						}
					}
				}
				if owned == 4 {
					catalog.Store(true)
				}
				n := calls.Add(1)
				output := []any{}
				if n == 1 {
					name, command := "mcp__swarm__swarm_shell", "env > tool-env; printf allowed > probe.txt"
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
						name = "mcp__swarm__host_shell"
						command = "printf escaped > forged"
					}
					if behavior == "cancellation" {
						command = "echo started > started; setsid sh -c 'sleep 2; echo escaped > survived' & wait"
					}
					args, _ := json.Marshal(map[string]string{"command": command})
					output = append(output, map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": name, "arguments": string(args), "status": "completed"})
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
			env := append(nativeEnv(home, false, Dependencies{}), "GH_TOKEN=synthetic-coordinator", "OPENAI_API_KEY=synthetic-provider", "SWARM_GITHUB_TOKEN_COMMAND=synthetic-command")
			var out process.Output
			runErr := runNativeCommand(ctx, binary, work, env, args, nil, &out, &out)
			// Codex and its MCP child must both finish before the launcher returns.
			pidBytes, pidErr := os.ReadFile(filepath.Join(control, "pid"))
			if pidErr != nil {
				t.Fatal("missing child pid", pidErr, out.String())
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
				t.Fatal("server never started", err, out.String())
			}
			for _, key := range []string{"GH_TOKEN=", "OPENAI_API_KEY=", "SWARM_GITHUB_TOKEN_COMMAND=", "SSH_AUTH_SOCK="} {
				if strings.Contains(string(captured), key) {
					t.Fatalf("server inherited credential %s", key)
				}
			}
			if !catalog.Load() {
				t.Fatal("initialize/list did not expose four owned tools", out.String())
			}
			if behavior == "cancellation" {
				if !<-started || runErr == nil {
					t.Fatal("tool cancellation not exercised", out.String())
				}
				time.Sleep(2200 * time.Millisecond)
				if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
					t.Fatal("tool descendant survived")
				}
				return
			}
			if behavior == "forged-host-tool" {
				if calls.Load() == 0 {
					t.Fatal("forged call not submitted")
				}
				if _, err := os.Stat(filepath.Join(work, "forged")); !os.IsNotExist(err) {
					t.Fatal("forged tool executed")
				}
				return
			}
			if runErr != nil {
				t.Fatal(runErr, out.String())
			}
			data, err := os.ReadFile(filepath.Join(work, "probe.txt"))
			if err != nil || string(data) != "allowed" || calls.Load() < 2 {
				t.Fatal("tool cycle failed", err, out.String())
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
			Command string
			Args    []string
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
	child.Env = []string{"GH_TOKEN=synthetic-coordinator", "OPENAI_API_KEY=synthetic-provider"}
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
		{"method": "initialize", "params": map[string]string{"protocolVersion": "2024-11-05"}},
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
	for _, key := range []string{"GH_TOKEN=", "OPENAI_API_KEY=", "SWARM_GITHUB_TOKEN_COMMAND=", "SSH_AUTH_SOCK="} {
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
