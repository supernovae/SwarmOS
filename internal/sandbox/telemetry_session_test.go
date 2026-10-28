package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTelemetrySessionIDWriterPassesThroughUnchanged(t *testing.T) {
	t.Parallel()
	const a, b = "5f0e8d7c-6b5a-4e3d-8c2b-1a0f9e8d7c6b", "9a8b7c6d-5e4f-4321-8fed-cba987654321"
	input := "OpenAI Codex v0.164.0\n--------\nworkdir: /w\nsession id: " + a + "\n" +
		"session id: " + strings.Repeat("x", sessionLineMax) + "session id: " + b + "\n" + // over-long line: dropped
		"session id: not-a-uuid\nprefix session id: " + b + "\nsession id: " + b + "\r\n" + "session id: " + a // unterminated: ignored
	for _, chunk := range []int{1, 7, 4096, len(input)} {
		var shown bytes.Buffer
		w := &sessionIDWriter{w: &shown}
		for rest := input; rest != ""; {
			n := min(chunk, len(rest))
			if written, err := w.Write([]byte(rest[:n])); err != nil || written != n {
				t.Fatalf("write = %d, %v", written, err)
			}
			rest = rest[n:]
		}
		if shown.String() != input {
			t.Fatalf("chunk %d: displayed stderr changed", chunk)
		}
		if want := []string{a, b}; !slices.Equal(w.ids, want) {
			t.Fatalf("chunk %d: ids = %q, want %q", chunk, w.ids, want)
		}
	}
	var many strings.Builder
	for i := 0; i < sessionIDsMax+5; i++ {
		fmt.Fprintf(&many, "session id: %08x-0000-4000-8000-000000000000\n", i)
	}
	w := &sessionIDWriter{w: io.Discard}
	_, _ = w.Write([]byte(many.String()))
	if len(w.ids) != sessionIDsMax {
		t.Fatalf("kept %d ids", len(w.ids))
	}
}

func TestTelemetryCappedBuffer(t *testing.T) {
	t.Parallel()
	b := &cappedBuffer{limit: 10}
	for _, chunk := range []string{"12345", "6789", "abcdef", "g"} {
		if n, err := b.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if !b.overflow || string(b.buf) != "123456789a" {
		t.Fatalf("buffer = %q overflow %v", b.buf, b.overflow)
	}
	exact := &cappedBuffer{limit: 3}
	_, _ = exact.Write([]byte("abc"))
	if exact.overflow || string(exact.buf) != "abc" {
		t.Fatalf("exact = %q overflow %v", exact.buf, exact.overflow)
	}
}

func TestTelemetryHelpLists(t *testing.T) {
	t.Parallel()
	help := "  --session-id <uuid>  Use a session\n  --session=<id>\n  -s, --sessionless\n  [--continue]"
	for flag, want := range map[string]bool{"--session-id": true, "--session": true, "--continue": true, "--sessionless": true, "--sess": false, "--no-session": false} {
		if got := helpLists(help, flag); got != want {
			t.Errorf("helpLists(%q) = %v", flag, got)
		}
	}
	if helpLists("  --session-id <uuid>", "--session") {
		t.Error("a longer flag matched")
	}
}

func TestTelemetryNilObserveSessionIsInertAndRecovers(t *testing.T) {
	t.Parallel()
	var l *Lifecycle
	l.observeSession(HarnessSession{Driver: "omp"})
	(&Lifecycle{}).observeSession(HarnessSession{Driver: "omp"})
	called := false
	(&Lifecycle{ObserveSession: func(HarnessSession) { called = true; panic("observer bug") }}).observeSession(HarnessSession{})
	if !called {
		t.Fatal("observer not called")
	}
}

// TestOpenCodeTelemetryHelper fakes the OpenCode CLI. It logs every api
// operation and run argv to the control directory, its working directory.
func TestOpenCodeTelemetryHelper(t *testing.T) {
	t.Parallel()
	index := slices.Index(os.Args, "--swarm-opencode-telemetry-fixture")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	mode := os.Getenv("SWARM_OPENCODE_FIXTURE_MODE")
	cwd, _ := os.Getwd()
	logLine := func(name, line string) {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(f, line)
		f.Close()
	}
	send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	switch args[0] {
	case "serve":
		send(map[string]any{"url": "http://127.0.0.1:12345"})
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "api":
		operation := args[3]
		logLine("api.log", strings.Join(args[3:], " "))
		switch operation {
		case "mcp.list":
			send(map[string]any{"data": []any{map[string]any{"name": "swarm", "status": map[string]string{"status": "connected"}}}})
		case "agent.get":
			send(map[string]any{"data": map[string]any{"id": "swarm", "permissions": []any{map[string]string{"Action": "*", "Resource": "*", "Effect": "deny"}, map[string]string{"Action": "swarm_swarm_*", "Resource": "*", "Effect": "allow"}}}})
		case "session.create":
			switch mode {
			case "create-fails":
				os.Exit(2)
			case "wrong-directory":
				send(map[string]any{"data": map[string]any{"id": "ses_RUN", "directory": cwd + "-other"}})
			case "bad-id":
				send(map[string]any{"data": map[string]any{"id": "ses/../RUN", "directory": cwd}})
			default:
				send(map[string]any{"data": map[string]any{"id": "ses_RUN", "directory": cwd}})
			}
		case "session.messages":
			switch mode {
			case "hang":
				time.Sleep(30 * time.Second)
			case "oversized":
				_, _ = os.Stdout.Write(bytes.Repeat([]byte(" "), telemetryRecordsMax+1))
			}
			send(map[string]any{"data": []any{map[string]any{"info": map[string]any{"id": "msg_1", "sessionID": "ses_RUN", "role": "assistant"}}}})
		default:
			os.Exit(2)
		}
	case "run":
		logLine("runs.log", strings.ReplaceAll(strings.Join(args, " "), "\n", `\n`))
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestOpenCodeTelemetrySessionScoped(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { openCodeTelemetryTimeout = d }(openCodeTelemetryTimeout)
	openCodeTelemetryTimeout = 3 * time.Second
	for _, tc := range []struct {
		mode        string
		sessionFlag bool
		tracked     bool // run turns use --session and messages are fetched
		records     bool
	}{
		{"ok", true, true, true},
		{"ok", false, false, false}, // run --help lacks --session
		{"create-fails", true, false, false},
		{"wrong-directory", true, false, false},
		{"bad-id", true, false, false},
		{"hang", true, true, false},
		{"oversized", true, true, false},
	} {
		t.Run(fmt.Sprint(tc.mode, tc.sessionFlag), func(t *testing.T) {
			t.Setenv("SWARM_OPENCODE_FIXTURE_MODE", tc.mode)
			binary := guidanceFixture(t, "#!/bin/sh\n# Protocol helpers have no background work at exit.\nexport GORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\"\nexec '"+strings.ReplaceAll(testBinary, "'", "'\"'\"'")+"' -test.run=TestOpenCodeTelemetryHelper -- --swarm-opencode-telemetry-fixture \"$@\"\n")
			control, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			env := append(nativeEnv(control, false, Dependencies{}), "SWARM_OPENCODE_FIXTURE_MODE="+tc.mode)
			session := HarnessSession{Driver: "opencode", Control: control}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			start := time.Now()
			err = runOpenCodeSession(ctx, binary, "", "initial task", control, env, tc.sessionFlag, &session, true, strings.NewReader("follow-up\n\n"), io.Discard, io.Discard, nil)
			if err != nil {
				t.Fatalf("telemetry changed the run outcome: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("post-run fetch stalled the run for %v", elapsed)
			}
			apiLog, _ := os.ReadFile(filepath.Join(control, "api.log"))
			runs, _ := os.ReadFile(filepath.Join(control, "runs.log"))
			turns := strings.Split(strings.TrimSpace(string(runs)), "\n")
			if len(turns) != 2 {
				t.Fatalf("turns = %q", turns)
			}
			wantOps := []string{"mcp.list", "agent.get --param agentID=swarm"}
			if tc.sessionFlag {
				wantOps = append(wantOps, "session.create")
			}
			if tc.tracked {
				wantOps = append(wantOps, "session.messages --param sessionID=ses_RUN")
			}
			if got := strings.Split(strings.TrimSpace(string(apiLog)), "\n"); !slices.Equal(got, wantOps) {
				t.Fatalf("api operations = %q, want %q", got, wantOps)
			}
			for i, turn := range turns {
				hasSession, hasContinue := strings.Contains(turn, "--session ses_RUN"), strings.Contains(turn, "--continue")
				if hasSession != tc.tracked || hasContinue != (!tc.tracked && i > 0) {
					t.Fatalf("turn %d argv = %q", i, turn)
				}
			}
			if tc.tracked != (len(session.SessionIDs) == 1) || tc.records != (session.Records != nil) {
				t.Fatalf("session = ids %q records %d bytes", session.SessionIDs, len(session.Records))
			}
			if tc.records && !strings.Contains(string(session.Records), `"sessionID":"ses_RUN"`) {
				t.Fatalf("records = %s", session.Records)
			}
		})
	}
}
