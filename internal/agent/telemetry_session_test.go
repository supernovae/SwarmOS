package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
)

// Fixtures under testdata/telemetry/sessions are labelled by harness version.
// Each SOURCE.md says whether the files were captured from that version or are
// hand-written to its expected shape (unverified-shape).

func usageSummaryR(u harnessUsage) string {
	return usageSummary(u.sessionUsage) + " reasoning=" + intValue(u.reasoning)
}

const unavailableHarnessUsage = unavailableUsage + " reasoning=nil"

const (
	claudeFixtureA = "7d5c2a8e-1f3b-4c6d-9e0a-2b4c6d8e0f12"
	claudeFixtureB = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	codexFixture1  = "5f0e8d7c-6b5a-4e3d-8c2b-1a0f9e8d7c6b"
	codexFixture2  = "9a8b7c6d-5e4f-4321-8fed-cba987654321"
	ompFixtureID   = "01a17fc9-4ef6-7133-adae-86a0077e2b9a"
	ompFixtureFile = "2026-10-27T23-54-21-814Z_" + ompFixtureID + ".jsonl"
	openCodeID     = "ses_SWARMRUN01"
)

// sessionFixture is one harness's run laid out in a fake host HOME.
type sessionFixture struct {
	session sandbox.HarnessSession
	files   []string // absolute paths of the run's own session files
	started time.Time
	ended   time.Time
}

func (f *sessionFixture) collect() harnessUsage {
	return collectHarnessSession(f.session, f.started, f.ended)
}

func readSessionFixture(t *testing.T, dir, name, control string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "telemetry", "sessions", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(data, []byte("/CONTROL"), []byte(control))
}

func writeSessionFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newSessionFixture(t *testing.T, driver string) *sessionFixture {
	t.Helper()
	home := t.TempDir()
	control := filepath.Join(t.TempDir(), "swarm-native-1", "control")
	now := time.Now()
	f := &sessionFixture{session: sandbox.HarnessSession{Driver: driver, Home: home, Control: control}, started: now.Add(-time.Minute), ended: now}
	switch driver {
	case "claude":
		dir := filepath.Join(home, ".claude", "projects", claudeProjectSlug(control))
		for _, id := range []string{claudeFixtureA, claudeFixtureB} {
			path := filepath.Join(dir, id+".jsonl")
			writeSessionFile(t, path, readSessionFixture(t, "claude-2.1.311", id+".jsonl", control))
			f.files = append(f.files, path)
			f.session.SessionIDs = append(f.session.SessionIDs, id)
		}
		// Another project's session must never be read.
		writeSessionFile(t, filepath.Join(home, ".claude", "projects", claudeProjectSlug(control)+"-x", claudeFixtureA+".jsonl"), []byte("{not json"))
	case "codex":
		dir := filepath.Join(home, ".codex", "sessions", now.Local().Format(filepath.Join("2006", "01", "02")))
		for _, id := range []string{codexFixture1, codexFixture2} {
			name := map[string]string{codexFixture1: "rollout-2026-10-27T10-00-00-", codexFixture2: "rollout-2026-10-27T10-05-00-"}[id] + id + ".jsonl"
			path := filepath.Join(dir, name)
			writeSessionFile(t, path, readSessionFixture(t, "codex-0.164.0", name, control))
			f.files = append(f.files, path)
			f.session.SessionIDs = append(f.session.SessionIDs, id)
		}
		// Other sessions on the same day are never opened.
		writeSessionFile(t, filepath.Join(dir, "rollout-2026-10-27T09-00-00-11111111-2222-4333-8444-555555555555.jsonl"), []byte("{not json"))
	case "omp":
		dir := filepath.Join(home, ".omp", "agent", "sessions", ompSessionSlug(control))
		path := filepath.Join(dir, ompFixtureFile)
		writeSessionFile(t, path, readSessionFixture(t, "omp-18.12.6", ompFixtureFile, control))
		// omp also creates a sibling directory with the same stem.
		if err := os.Mkdir(strings.TrimSuffix(path, ".jsonl"), 0o700); err != nil {
			t.Fatal(err)
		}
		f.files = []string{path}
		writeSessionFile(t, filepath.Join(home, ".omp", "agent", "sessions", ompSessionSlug(control)+"-x", ompFixtureFile), []byte("{not json"))
	case "opencode":
		f.session.SessionIDs = []string{openCodeID}
		f.session.Records = readSessionFixture(t, "opencode-2.1.2", "messages.json", control)
	}
	return f
}

func TestTelemetryHarnessSessions(t *testing.T) {
	for _, tc := range []struct{ driver, want string }{
		// claude 2.1.311 (unverified-shape): input excludes cache reads; no reasoning.
		{"claude", "in=25 out=90 cached=25000 calls=2 repeats=1 model=claude-sonnet-5-5 effort=nil reasoning=nil"},
		// codex 0.164.0 (unverified-shape): last snapshot per turn, summed across turns.
		{"codex", "in=8000 out=250 cached=5500 calls=2 repeats=1 model=gpt-5.5-codex effort=high reasoning=120"},
		// omp 18.12.6 (captured): 3 x (602 in, 22 out, 400 cacheRead, 7 reasoning).
		{"omp", "in=1806 out=66 cached=1200 calls=2 repeats=1 model=fake-model effort=nil reasoning=21"},
		// opencode 2.1.2 (unverified-shape).
		{"opencode", "in=60 out=90 cached=13000 calls=2 repeats=1 model=claude-sonnet-5-5 effort=nil reasoning=20"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			if got := usageSummaryR(newSessionFixture(t, tc.driver).collect()); got != tc.want {
				t.Fatalf("usage = %s\nwant    %s", got, tc.want)
			}
		})
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func rewriteFile(t *testing.T, path, old, replacement string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("%s lacks %q", path, old)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte(old), []byte(replacement), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryHarnessSessionIsolation(t *testing.T) {
	type mutation func(*testing.T, *sessionFixture)
	// Shared file-based cases; each must make every value unavailable.
	common := map[string]mutation{
		"absent": func(t *testing.T, f *sessionFixture) {
			for _, path := range f.files {
				os.Remove(path)
			}
		},
		"malformed": func(t *testing.T, f *sessionFixture) { appendFile(t, f.files[0], []byte("{not json\n")) },
		"oversized": func(t *testing.T, f *sessionFixture) {
			appendFile(t, f.files[0], bytes.Repeat([]byte("\n"), telemetryMaxBytes+1))
		},
		"long line": func(t *testing.T, f *sessionFixture) {
			appendFile(t, f.files[0], append(bytes.Repeat([]byte(" "), telemetryMaxLineBytes+1), '\n'))
		},
		"empty home": func(t *testing.T, f *sessionFixture) { f.session.Home = "" },
		"relative home": func(t *testing.T, f *sessionFixture) {
			f.session.Home = "home"
		},
		"decoy project only": func(t *testing.T, f *sessionFixture) {
			for _, path := range f.files {
				dir := filepath.Dir(path)
				if f.session.Driver == "codex" {
					dir = filepath.Join(f.session.Home, ".codex", "other")
				} else {
					dir += "-y"
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(dir, filepath.Base(path))); err != nil {
					t.Fatal(err)
				}
			}
		},
	}
	specific := map[string]map[string]mutation{
		"claude": {
			"other session id": func(t *testing.T, f *sessionFixture) {
				f.session.SessionIDs[0] = "11111111-2222-4333-8444-555555555555"
			},
			"non-uuid id":      func(t *testing.T, f *sessionFixture) { f.session.SessionIDs[0] = "../" + claudeFixtureA },
			"no session ids":   func(t *testing.T, f *sessionFixture) { f.session.SessionIDs = nil },
			"other control":    func(t *testing.T, f *sessionFixture) { f.session.Control += "x" },
			"relative control": func(t *testing.T, f *sessionFixture) { f.session.Control = "control" },
		},
		"codex": {
			"other session id": func(t *testing.T, f *sessionFixture) {
				f.session.SessionIDs[1] = "11111111-2222-4333-8444-666666666666"
			},
			"non-uuid id": func(t *testing.T, f *sessionFixture) { f.session.SessionIDs[0] = "*" },
			"ambiguous": func(t *testing.T, f *sessionFixture) {
				data, _ := os.ReadFile(f.files[0])
				writeSessionFile(t, filepath.Join(filepath.Dir(f.files[0]), "rollout-copy-"+codexFixture1+".jsonl"), data)
			},
			"run outside window": func(t *testing.T, f *sessionFixture) {
				f.started, f.ended = f.started.Add(-72*time.Hour), f.ended.Add(-72*time.Hour)
			},
			"ended before started": func(t *testing.T, f *sessionFixture) { f.ended = f.started.Add(-time.Second) },
			"codex home elsewhere": func(t *testing.T, f *sessionFixture) { f.session.CodexHome = t.TempDir() },
		},
		"omp": {
			"ambiguous": func(t *testing.T, f *sessionFixture) {
				data, _ := os.ReadFile(f.files[0])
				writeSessionFile(t, filepath.Join(filepath.Dir(f.files[0]), "2026-10-28T00-00-00-000Z_"+ompFixtureID+".jsonl"), data)
			},
			"header cwd differs": func(t *testing.T, f *sessionFixture) {
				rewriteFile(t, f.files[0], `"cwd":"`+f.session.Control+`"`, `"cwd":"`+f.session.Control+`-x"`)
			},
			"header id differs": func(t *testing.T, f *sessionFixture) {
				rewriteFile(t, f.files[0], `"id":"`+ompFixtureID+`"`, `"id":"01a17fc9-0000-7133-adae-86a0077e2b9a"`)
			},
			"no header": func(t *testing.T, f *sessionFixture) {
				rewriteFile(t, f.files[0], `"type":"session"`, `"type":"other"`)
			},
			"other control": func(t *testing.T, f *sessionFixture) { f.session.Control += "x" },
		},
	}
	for _, driver := range []string{"claude", "codex", "omp"} {
		cases := map[string]mutation{}
		for name, m := range common {
			cases[name] = m
		}
		for name, m := range specific[driver] {
			cases[name] = m
		}
		for name, mutate := range cases {
			t.Run(driver+"/"+name, func(t *testing.T) {
				f := newSessionFixture(t, driver)
				mutate(t, f)
				if got := usageSummaryR(f.collect()); got != unavailableHarnessUsage {
					t.Fatalf("usage = %s", got)
				}
			})
		}
	}
	openCode := map[string]func(*sandbox.HarnessSession){
		"no records": func(s *sandbox.HarnessSession) { s.Records = nil },
		"malformed":  func(s *sandbox.HarnessSession) { s.Records = s.Records[:len(s.Records)/2] },
		"oversized": func(s *sandbox.HarnessSession) {
			s.Records = append(s.Records, bytes.Repeat([]byte(" "), telemetryMaxBytes)...)
		},
		"no data":         func(s *sandbox.HarnessSession) { s.Records = []byte(`{"error":"x"}`) },
		"no session id":   func(s *sandbox.HarnessSession) { s.SessionIDs = nil },
		"two session ids": func(s *sandbox.HarnessSession) { s.SessionIDs = append(s.SessionIDs, "ses_other") },
		"bad session id":  func(s *sandbox.HarnessSession) { s.SessionIDs = []string{"ses/../x"} },
		"decoy message": func(s *sandbox.HarnessSession) {
			s.Records = bytes.Replace(s.Records, []byte(`"id":"msg_a2","sessionID":"`+openCodeID+`"`), []byte(`"id":"msg_a2","sessionID":"ses_OTHER"`), 1)
		},
		"decoy part": func(s *sandbox.HarnessSession) {
			s.Records = bytes.Replace(s.Records, []byte(`"id":"prt_a3","sessionID":"`+openCodeID+`"`), []byte(`"id":"prt_a3","sessionID":"ses_OTHER"`), 1)
		},
		"message without session": func(s *sandbox.HarnessSession) {
			s.Records = bytes.Replace(s.Records, []byte(`"id":"msg_u1","sessionID":"`+openCodeID+`",`), []byte(`"id":"msg_u1",`), 1)
		},
		"other session id": func(s *sandbox.HarnessSession) { s.SessionIDs = []string{"ses_OTHER"} },
	}
	for name, mutate := range openCode {
		t.Run("opencode/"+name, func(t *testing.T) {
			f := newSessionFixture(t, "opencode")
			before := string(f.session.Records)
			mutate(&f.session)
			if name != "no records" && name != "oversized" && name != "no data" && name != "no session id" && name != "two session ids" && name != "bad session id" && name != "other session id" && string(f.session.Records) == before {
				t.Fatal("mutation did not apply")
			}
			if got := usageSummaryR(f.collect()); got != unavailableHarnessUsage {
				t.Fatalf("usage = %s", got)
			}
		})
	}
	t.Run("unknown driver", func(t *testing.T) {
		f := newSessionFixture(t, "omp")
		f.session.Driver = "gemini"
		if got := usageSummaryR(f.collect()); got != unavailableHarnessUsage {
			t.Fatalf("usage = %s", got)
		}
	})
}

// Negative or overflowing counts leave the token values unavailable rather
// than reporting a partial or wrapped total.
func TestTelemetryHarnessSessionRejectsBadCounts(t *testing.T) {
	for _, tc := range []struct{ driver, old, replacement string }{
		{"claude", `"output_tokens":40`, `"output_tokens":-40`},
		{"codex", `"output_tokens":50,"reasoning_output_tokens":10`, `"output_tokens":-1,"reasoning_output_tokens":10`},
		{"omp", `"input":602`, `"input":-602`},
		{"omp", `"input":602`, `"input":9007199254740993`},
		{"opencode", `"input":40`, `"input":-40`},
	} {
		t.Run(tc.driver+tc.replacement, func(t *testing.T) {
			f := newSessionFixture(t, tc.driver)
			if tc.driver == "opencode" {
				f.session.Records = bytes.Replace(f.session.Records, []byte(tc.old), []byte(tc.replacement), 1)
			} else {
				path := f.files[0]
				if tc.driver == "codex" {
					path = f.files[1]
				}
				rewriteFile(t, path, tc.old, tc.replacement)
			}
			u := f.collect()
			if u.input != nil || u.output != nil || u.cached != nil || u.reasoning != nil {
				t.Fatalf("bad count produced tokens: %s", usageSummaryR(u))
			}
		})
	}
}

// A run that leaves no session at the derived location reads nothing else,
// even when every other location under HOME holds plausible sessions.
func TestTelemetryHarnessSessionReadsOnlyDerivedPaths(t *testing.T) {
	for _, driver := range []string{"claude", "codex", "omp"} {
		t.Run(driver, func(t *testing.T) {
			f := newSessionFixture(t, driver)
			want := usageSummaryR(f.collect())
			if want == unavailableHarnessUsage {
				t.Fatal("fixture produced no values")
			}
			// Canary copies of the run's own data elsewhere under HOME.
			for _, dir := range []string{".claude/projects/other", ".codex/sessions/1999/01/01", ".omp/agent/sessions/other", ".local/share/opencode"} {
				for _, path := range f.files {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					writeSessionFile(t, filepath.Join(f.session.Home, dir, filepath.Base(path)), bytes.ReplaceAll(data, []byte(`"output`), []byte(`"canary`)))
				}
			}
			if got := usageSummaryR(f.collect()); got != want {
				t.Fatalf("canaries changed the result: %s, want %s", got, want)
			}
		})
	}
}

// Host-session values reach the recorded telemetry, take precedence over the
// empty scratch HOME and include reasoning; other drivers' sessions are ignored.
func TestTelemetryObservationUsesHarnessSession(t *testing.T) {
	f := newSessionFixture(t, "omp")
	o := newRunObservation("omp")
	o.lifecycle.ObserveSession(f.session)
	o.lifecycle.ObserveScratch(t.TempDir())
	tel := o.telemetry("developer", "omp")
	if err := tel.Validate(); err != nil {
		t.Fatal(err)
	}
	if intValue(tel.InputTokens) != "1806" || intValue(tel.ReasoningTokens) != "21" || strValue(tel.ActualModel) != "fake-model" || tel.UsageSource != contract.UsageSourceSessionLog {
		t.Fatalf("telemetry = %+v", tel)
	}
	encoded, err := tel.Encode()
	if err != nil || !strings.Contains(encoded, `"reasoning_tokens":21`) {
		t.Fatalf("encoded = %s, %v", encoded, err)
	}

	other := newRunObservation("claude")
	other.lifecycle.ObserveSession(f.session)
	if tel := other.telemetry("developer", "claude"); tel.InputTokens != nil || tel.UsageSource != contract.UsageSourceNone {
		t.Fatalf("foreign driver session was used: %+v", tel)
	}
}

// TestTelemetryHarnessHelper is a fake delegated harness. It speaks just enough
// of each CLI interface and writes its session where the real harness would.
func TestTelemetryHarnessHelper(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--swarm-telemetry-fixture" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	driver, args := os.Args[index+1], os.Args[index+2:]
	cwd, _ := os.Getwd()
	home := os.Getenv("HOME")
	session := func() []byte {
		data, err := os.ReadFile(os.Getenv("SWARM_TELEMETRY_FIXTURE"))
		if err != nil {
			os.Exit(3)
		}
		return bytes.ReplaceAll(data, []byte("/CONTROL"), []byte(cwd))
	}
	write := func(path string, data []byte) {
		if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, data, 0o600) != nil {
			os.Exit(3)
		}
	}
	switch {
	case args[0] == "--version" && driver == "omp":
		fmt.Println("omp/18.12.6")
	case args[0] == "--version":
		fmt.Println("2.1.311 (Claude Code)")
	case args[0] == "--help":
		fmt.Println("--restricted --strict-mcp-config --tools --settings --permission-mode --session-id <uuid> --mode --no-tools --no-extensions --no-rules --config --no-session")
	case args[0] == "auth":
		fmt.Println(`{"loggedIn":true,"authMethod":"fixture"}`)
	case driver == "claude":
		var id string
		for i, arg := range args {
			if arg == "--session-id" && i+1 < len(args) {
				id = args[i+1]
			}
		}
		if id == "" {
			os.Exit(4)
		}
		write(filepath.Join(home, ".claude", "projects", claudeProjectSlug(cwd), id+".jsonl"), session())
		fmt.Println("claude fixture done")
	case driver == "omp":
		write(filepath.Join(home, "omp-args.txt"), []byte(strings.Join(args, "\n")))
		send := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
		send(map[string]string{"type": "ready"})
		decoder := json.NewDecoder(os.Stdin)
		var names []string
		for {
			var frame struct {
				Type, ID string
				Tools    []struct{ Name string }
			}
			if decoder.Decode(&frame) != nil {
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
				write(filepath.Join(home, ".omp", "agent", "sessions", ompSessionSlug(cwd), ompFixtureFile), session())
				response = map[string]any{"type": "prompt_result", "id": frame.ID, "status": "completed", "agentInvoked": true, "sessionSettled": true}
			}
			send(response)
		}
	}
	os.Exit(0)
}

// A real RunDelegatedHarness call reads the run's own session from the host
// HOME before cleanup, and a malformed or oversized session leaves the run's
// result and error exactly as without telemetry.
func TestTelemetryDelegatedRunObservesSession(t *testing.T) {
	if err := sandbox.CheckToolExecutor(context.Background()); err != nil {
		t.Skipf("isolated tool executor unavailable here: %v", err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"claude": filepath.Join("testdata", "telemetry", "sessions", "claude-2.1.311", claudeFixtureA+".jsonl"),
		"omp":    filepath.Join("testdata", "telemetry", "sessions", "omp-18.12.6", ompFixtureFile),
	}
	wants := map[string]string{
		"claude": "in=20 out=70 cached=21000 calls=2 repeats=1 model=claude-sonnet-5-5 effort=nil reasoning=nil",
		"omp":    "in=1806 out=66 cached=1200 calls=2 repeats=1 model=fake-model effort=nil reasoning=21",
	}
	for _, driver := range []string{"claude", "omp"} {
		for _, damage := range []string{"", "malformed", "oversized"} {
			t.Run(driver+"/"+damage, func(t *testing.T) {
				fixture, err := filepath.Abs(fixtures[driver])
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "malformed":
					data = append(data, "{not json\n"...)
				case "oversized":
					data = append(data, bytes.Repeat([]byte("\n"), telemetryMaxBytes+1)...)
				}
				fixture = filepath.Join(t.TempDir(), "session.jsonl")
				if err := os.WriteFile(fixture, data, 0o600); err != nil {
					t.Fatal(err)
				}
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("SWARM_TELEMETRY_FIXTURE", fixture)
				binary := filepath.Join(t.TempDir(), driver)
				script := "#!/bin/sh\nSWARM_TELEMETRY_FIXTURE='" + fixture + "' exec '" + strings.ReplaceAll(testBinary, "'", "'\"'\"'") + "' -test.run=TestTelemetryHarnessHelper -- --swarm-telemetry-fixture " + driver + " \"$@\"\n"
				if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				o := newRunObservation(driver)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				var output bytes.Buffer
				_, _, runErr := sandbox.RunDelegatedHarness(ctx, t.TempDir(), driver, binary, "", "task", sandbox.WorkerOptions{Role: "developer", Lifecycle: &o.lifecycle, DiscardChanges: true}, false, nil, &output, io.Discard)
				if runErr != nil {
					t.Fatalf("run failed: %v\n%s", runErr, output.String())
				}
				if o.session == nil {
					t.Fatal("session was not observed")
				}
				want := wants[driver]
				if damage != "" {
					want = unavailableHarnessUsage
				}
				if got := usageSummaryR(*o.session); got != want {
					t.Fatalf("usage = %s\nwant    %s", got, want)
				}
				if driver == "omp" {
					args, err := os.ReadFile(filepath.Join(home, "omp-args.txt"))
					if err != nil || strings.Contains(string(args), "--no-session") {
						t.Fatalf("omp argv = %q, %v", args, err)
					}
				}
			})
		}
	}
}
