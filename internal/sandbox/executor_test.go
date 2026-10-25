package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestDelegatedValidationWithoutCodex(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for actual delegated validation")
	}
	cfg := &contract.SwarmConfig{DefaultAgent: "worker", Agents: map[string]contract.AgentConfig{"worker": {Harness: "omp", Binary: "not-used-by-validation"}}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := RunConfiguredNativeValidation(ctx, dir, cfg, "test \"$(cat source)\" = original; echo changed > source", &out, &out); err != nil {
		t.Fatal(err, out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "source"))
	if err != nil || string(data) != "original" {
		t.Fatal("validation changed coordinator workspace", err, string(data))
	}
}

func TestMCPToolCancellation(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for actual tool cancellation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	work := t.TempDir()
	requests, input := io.Pipe()
	output, responses := io.Pipe()
	defer input.Close()
	defer output.Close()
	done := make(chan error, 1)
	go func() { done <- ServeTools(ctx, ToolExecutor{Work: work}, requests, responses) }()
	encoder := json.NewEncoder(input)
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "swarm_shell", "arguments": map[string]string{"command": "echo ready > ready; sleep 2; echo escaped > survived"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(work, "ready")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("tool did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 1}}); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result ToolResult `json:"result"`
	}
	if err := json.NewDecoder(output).Decode(&response); err != nil || !response.Result.IsError {
		t.Fatal("cancelled tool did not fail", err, response)
	}
	input.Close()
	<-done
	if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
		t.Fatal("cancelled tool continued")
	}
}

func TestToolExecutorBoundary(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for real Bubblewrap tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CheckToolExecutor(ctx); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	host := t.TempDir()
	secret := filepath.Join(host, "coordinator-token")
	os.WriteFile(secret, []byte("dummy-credential"), 0600)
	os.MkdirAll(filepath.Join(work, ".swarm/active"), 0700)
	os.Symlink(secret, filepath.Join(work, "escape"))
	e := ToolExecutor{Work: work}
	call := func(name string, args map[string]string, fail bool) ToolResult {
		data, _ := json.Marshal(args)
		result := e.Call(ctx, name, data)
		if result.IsError != fail {
			t.Fatalf("%s: %+v", name, result)
		}
		return result
	}
	call("swarm_read", map[string]string{"path": secret}, true)
	call("swarm_read", map[string]string{"path": "escape"}, true)
	// The same /tmp pathname may be created in the private tmpfs, but it must
	// not touch the host file (checked below).
	call("swarm_write", map[string]string{"path": secret, "content": "stolen"}, false)
	call("swarm_write", map[string]string{"path": "hello.txt", "content": "hello"}, false)
	call("swarm_edit", map[string]string{"path": "hello.txt", "old_text": "hello", "new_text": "world"}, false)
	if result := call("swarm_read", map[string]string{"path": "hello.txt"}, false); result.Content[0].Text != "world" {
		t.Fatal(result)
	}
	e.ReadOnly = true
	call("swarm_write", map[string]string{"path": "hello.txt", "content": "changed"}, true)
	call("swarm_write", map[string]string{"path": ".swarm/active/REVIEW.md", "content": "review"}, false)
	data, _ := os.ReadFile(secret)
	if string(data) != "dummy-credential" {
		t.Fatal("host credential changed")
	}
	// Detached process groups must also end when the PID namespace exits.
	e.ReadOnly = false
	short, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	var output strings.Builder
	err := e.Run(short, `echo started > started; setsid sh -c 'sleep 1; echo survived > survived' & wait`, &output, &output)
	if err == nil || short.Err() == nil {
		t.Fatal("cancellation failed", err)
	}
	if _, err := os.Stat(filepath.Join(work, "started")); err != nil {
		t.Fatal("command never started", err, output.String())
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(work, "survived")); !os.IsNotExist(err) {
		t.Fatal("detached child survived")
	}
}

func TestOMPRealToolRegistration(t *testing.T) {
	binary := os.Getenv("SWARM_TEST_OMP_BINARY")
	if binary == "" {
		t.Skip("set SWARM_TEST_OMP_BINARY for real RPC interface qualification")
	}
	home := t.TempDir()
	control := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	version, err := InspectDelegatedHarness(ctx, "omp", binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("OMP", version)
	var output strings.Builder
	env := append(nativeEnv(home, false), "OPENAI_API_KEY=synthetic-offline-fixture")
	if err := runOMP(ctx, binary, "openai/gpt-4.1", "", control, env, ToolExecutor{Work: t.TempDir()}, false, nil, &output, &output); err != nil {
		t.Fatal(err, output.String())
	}
}

func TestMCPDoesNotExposeCoordinatorOperations(t *testing.T) {
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"approve\"}\n")
	var output strings.Builder
	if err := ServeTools(context.Background(), ToolExecutor{}, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "swarm_shell") || !strings.Contains(output.String(), "-32601") {
		t.Fatal(output.String())
	}
	result := (ToolExecutor{}).Call(context.Background(), "publish", json.RawMessage(`{}`))
	if !result.IsError {
		t.Fatal("unregistered tool accepted")
	}
}

func TestToolServerFollowsCoordinatorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requests, input := io.Pipe()
	defer input.Close()
	done := make(chan error, 1)
	go func() { done <- ServeTools(ctx, ToolExecutor{}, requests, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tool server ignored coordinator cancellation")
	}
}
