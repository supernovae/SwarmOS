package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These executables emulate harness protocols to test coordinator input
// plumbing. Boundary qualification has its own unchanged adversarial tests.
func guidanceFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness")
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestNativeAndClaudeFollowupGuidance(t *testing.T) {
	t.Parallel()
	for _, trim := range []bool{true, false} {
		for _, input := range []string{"follow-up\nsecond message\n\n", "\n", "", strings.Repeat("x", (64<<10)+1)} {
			t.Run(fmt.Sprint(trim, len(input)), func(t *testing.T) {
				t.Parallel()
				var messages, turns []string
				err := runFollowupTurns(t.Context(), "initial task", "continue: ", true, trim, strings.NewReader(input), io.Discard, func(prompt string) error { turns = append(turns, prompt); return nil }, func(message string) { messages = append(messages, message) })
				if len(input) > 64<<10 {
					if err == nil || len(messages) != 0 {
						t.Fatal("rejected input recorded")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				if strings.HasPrefix(input, "follow-up") {
					want = []string{"follow-up", "second message"}
				}
				if !reflect.DeepEqual(messages, want) {
					t.Fatalf("got %q want %q", messages, want)
				}
				if len(turns) != len(messages)+1 {
					t.Fatalf("delivery mismatch: %q %q", turns, messages)
				}
				for i, message := range messages {
					if turns[i+1] != "continue: "+message {
						t.Fatal("message not forwarded exactly once")
					}
				}
			})
		}
	}
}
func TestProtocolFollowupGuidance(t *testing.T) {
	t.Parallel()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{}
	for _, driver := range []string{"omp", "opencode"} {
		scripts[driver] = "#!/bin/sh\n# Protocol helpers have no background work at exit.\nexport GORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\"\nexec '" + strings.ReplaceAll(testBinary, "'", "'\"'\"'") + "' -test.run=TestGuidanceProtocolHelper -- --swarm-guidance-fixture " + driver + " \"$@\"\n"
	}

	for driver, script := range scripts {
		for _, input := range []string{"follow-up\nsecond message\n\n", "\n", ""} {
			t.Run(driver+input, func(t *testing.T) {
				t.Parallel()
				binary := guidanceFixture(t, script)
				control := t.TempDir()
				var messages []string
				callback := func(s string) { messages = append(messages, s) }
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var err error
				if driver == "omp" {
					err = runOMP(ctx, binary, "", "initial task", control, nativeEnv(control, false, Dependencies{}), ToolExecutor{Work: t.TempDir()}, true, strings.NewReader(input), io.Discard, io.Discard, callback)
				} else {
					err = runOpenCode(ctx, binary, "", "initial task", control, nativeEnv(control, false, Dependencies{}), true, strings.NewReader(input), io.Discard, io.Discard, callback)
				}
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				if strings.HasPrefix(input, "follow-up") {
					want = []string{"follow-up", "second message"}
				}
				if !reflect.DeepEqual(messages, want) {
					t.Fatalf("got %q want %q", messages, want)
				}
				data, err := os.ReadFile(filepath.Join(control, "delivered.txt"))
				if err != nil {
					t.Fatal(err)
				}
				for _, message := range messages {
					if strings.Count(string(data), message) != 1 {
						t.Fatalf("not forwarded once: %s", data)
					}
				}
			})
		}
	}
}

// A subprocess of this test binary emulates only the harness wire protocol.
func TestGuidanceProtocolHelper(t *testing.T) {
	t.Parallel()
	index := -1
	for i, arg := range os.Args {
		if arg == "--swarm-guidance-fixture" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	driver, args := os.Args[index+1], os.Args[index+2:]
	send := func(value any) {
		if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
			os.Exit(2)
		}
	}
	record := func(prompt string) {
		f, err := os.OpenFile("delivered.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(2)
		}
		_, err = f.WriteString(prompt + "\n")
		f.Close()
		if err != nil {
			os.Exit(2)
		}
	}
	if driver == "opencode" {
		switch args[0] {
		case "serve":
			send(map[string]any{"url": "http://127.0.0.1:12345"})
			_, _ = io.Copy(io.Discard, os.Stdin)
		case "api":
			switch args[3] {
			case "mcp.list":
				send(map[string]any{"data": []any{map[string]any{"name": "swarm", "status": map[string]string{"status": "connected"}}}})
			case "agent.get":
				send(map[string]any{"data": map[string]any{"id": "swarm", "permissions": []any{map[string]string{"Action": "*", "Resource": "*", "Effect": "deny"}, map[string]string{"Action": "swarm_swarm_*", "Resource": "*", "Effect": "allow"}}}})
			default:
				os.Exit(2)
			}
		case "run":
			record(args[len(args)-1])
		default:
			os.Exit(2)
		}
	} else {
		send(map[string]string{"type": "ready"})
		decoder := json.NewDecoder(os.Stdin)
		var names []string
		for {
			var frame struct {
				Type, ID, Message string
				Tools             []struct{ Name string }
			}
			if err := decoder.Decode(&frame); err != nil {
				break
			}
			response := map[string]any{"type": "response", "id": frame.ID, "success": true}
			switch frame.Type {
			case "set_host_tools":
				for _, tool := range frame.Tools {
					names = append(names, tool.Name)
				}
				response["data"] = map[string]any{"toolNames": names}
			case "get_state":
				tools := []map[string]string{}
				for _, name := range names {
					tools = append(tools, map[string]string{"name": name})
				}
				response["data"] = map[string]any{"dumpTools": tools}
			case "prompt":
				record(frame.Message)
				response = map[string]any{"type": "prompt_result", "id": frame.ID, "status": "completed", "agentInvoked": true, "sessionSettled": true}
			default:
				os.Exit(2)
			}
			send(response)
		}
	}
	os.Exit(0)
}
