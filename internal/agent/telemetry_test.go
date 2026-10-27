package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// The fixtures under testdata/telemetry are synthetic format samples, not
// captured CLI output; see testdata/telemetry/README.md.

func copyTelemetryFixture(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.CopyFS(home, os.DirFS(filepath.Join("testdata", "telemetry", name))); err != nil {
		t.Fatal(err)
	}
	return home
}

func intValue(v *int64) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprint(*v)
}

func strValue(v *string) string {
	if v == nil {
		return "nil"
	}
	return *v
}

func usageSummary(u sessionUsage) string {
	return fmt.Sprintf("in=%s out=%s cached=%s calls=%s repeats=%s model=%s effort=%s",
		intValue(u.input), intValue(u.output), intValue(u.cached), intValue(u.toolCalls), intValue(u.repeated), strValue(u.model), strValue(u.effort))
}

const unavailableUsage = "in=nil out=nil cached=nil calls=nil repeats=nil model=nil effort=nil"

func TestTelemetrySyntheticSessionLogs(t *testing.T) {
	for _, tc := range []struct{ driver, want string }{
		// Claude: the last usage per streamed message id, summed across messages;
		// the second Bash call repeats the first; no effort field is reported.
		{"claude", "in=170 out=60 cached=3500 calls=3 repeats=1 model=claude-sonnet-4-5 effort=nil"},
		// Codex: each rollout's last cumulative snapshot, summed across the two
		// ephemeral turn files; null token_count info is ignored.
		{"codex", "in=3200 out=150 cached=2600 calls=3 repeats=1 model=gpt-5-codex effort=high"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			if got := usageSummary(collectSessionUsage(tc.driver, copyTelemetryFixture(t, tc.driver))); got != tc.want {
				t.Fatalf("usage = %s, want %s", got, tc.want)
			}
		})
	}
}

func writeLog(t *testing.T, home, rel, body string) {
	t.Helper()
	path := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetrySessionLogEdgeCases(t *testing.T) {
	claude := func(id, model string, in, out int) string {
		return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n", id, model, in, out)
	}
	codexTotal := func(in, out int) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d}}}}`+"\n", in, out)
	}
	for _, tc := range []struct {
		name, driver string
		files        map[string]string
		want         string
	}{
		{"absent logs", "claude", nil, unavailableUsage},
		{"unsupported harness", "opencode", map[string]string{".claude/projects/p/s.jsonl": claude("m1", "x", 1, 1)}, unavailableUsage},
		{"malformed record", "claude", map[string]string{".claude/projects/p/s.jsonl": claude("m1", "x", 1, 1) + "{not json\n"}, unavailableUsage},
		{"truncated record", "codex", map[string]string{".codex/sessions/s.jsonl": `{"type":"event_msg","payload":{"type":"tok`}, unavailableUsage},
		{"message without id", "claude", map[string]string{".claude/projects/p/s.jsonl": claude("", "x", 1, 1)}, unavailableUsage},
		{"tool call without id", "codex", map[string]string{".codex/sessions/s.jsonl": `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}` + "\n"}, unavailableUsage},
		{"decreasing cumulative snapshot", "codex", map[string]string{".codex/sessions/s.jsonl": codexTotal(10, 5) + codexTotal(9, 6)}, unavailableUsage},
		{"no assistant records", "claude", map[string]string{".claude/projects/p/s.jsonl": `{"type":"user","message":{"content":"hi"}}` + "\n"}, unavailableUsage},
		{"ambiguous model and missing cache field", "claude", map[string]string{".claude/projects/p/s.jsonl": claude("m1", "model-a", 3, 4) + claude("m2", "model-b", 5, 6)},
			"in=8 out=10 cached=nil calls=0 repeats=0 model=nil effort=nil"},
		{"duplicate cumulative snapshots are not summed", "codex", map[string]string{".codex/sessions/s.jsonl": codexTotal(10, 5) + codexTotal(10, 5)},
			"in=10 out=5 cached=nil calls=0 repeats=0 model=nil effort=nil"},
		{"turn without token report", "codex", map[string]string{".codex/sessions/a.jsonl": codexTotal(10, 5), ".codex/sessions/b.jsonl": `{"type":"turn_context","payload":{"model":"m"}}` + "\n"},
			"in=nil out=nil cached=nil calls=0 repeats=0 model=m effort=nil"},
		{"unsafe model label", "claude", map[string]string{".claude/projects/p/s.jsonl": `{"type":"assistant","message":{"id":"m1","model":"evil|\u001b[31m","content":[],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"},
			"in=1 out=2 cached=nil calls=0 repeats=0 model=nil effort=nil"},
		{"line over limit", "claude", map[string]string{".claude/projects/p/s.jsonl": `{"type":"summary","summary":"` + strings.Repeat("a", telemetryMaxLineBytes) + `"}` + "\n"}, unavailableUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for rel, body := range tc.files {
				writeLog(t, home, rel, body)
			}
			if got := usageSummary(collectSessionUsage(tc.driver, home)); got != tc.want {
				t.Fatalf("usage = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTelemetrySessionLogLimits(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m","model":"x","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"too many files", func(t *testing.T, home string) {
			for i := 0; i <= telemetryMaxFiles; i++ {
				writeLog(t, home, fmt.Sprintf(".claude/projects/p/%02d.jsonl", i), line)
			}
		}},
		{"too many entries", func(t *testing.T, home string) {
			for i := 0; i <= telemetryMaxEntries; i++ {
				writeLog(t, home, fmt.Sprintf(".claude/projects/p/%03d.txt", i), "x")
			}
		}},
		{"too deep", func(t *testing.T, home string) {
			writeLog(t, home, ".claude/projects/"+strings.Repeat("d/", telemetryMaxDepth+1)+"s.jsonl", line)
		}},
		{"aggregate bytes", func(t *testing.T, home string) {
			chunk := strings.Repeat(line, (telemetryMaxBytes/len(line))/4+1)
			for i := 0; i < 5; i++ {
				writeLog(t, home, fmt.Sprintf(".claude/projects/p/%d.jsonl", i), chunk)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, home)
			if got := usageSummary(collectSessionUsage("claude", home)); got != unavailableUsage {
				t.Fatalf("limit breach published %s", got)
			}
		})
	}
}

// TestTelemetrySessionHelper is a fake harness: it writes a session log into
// its own HOME, optionally a decoy into an unrelated directory, and prints
// ordinary stdout. It is gated by argv so it never runs on its own.
func TestTelemetrySessionHelper(t *testing.T) {
	if !slices.Contains(os.Args, "--swarm-telemetry-helper") {
		return
	}
	record := `{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-5","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3}}}` + "\n"
	target := os.Getenv("HOME")
	if decoy := os.Getenv("SWARM_TELEMETRY_DECOY"); decoy != "" {
		target = decoy
	}
	dir := filepath.Join(target, ".claude", "projects", "p")
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(record), 0o600) != nil {
		os.Exit(31)
	}
	fmt.Print("helper stdout\n")
	os.Exit(0)
}

func TestTelemetryAdapterReadsOnlyRunScratchHome(t *testing.T) {
	decoy := t.TempDir()
	for _, tc := range []struct {
		name  string
		decoy bool
		want  string
	}{
		{"scratch home log", false, "in=11 out=7 cached=3 calls=1 repeats=0 model=claude-opus-4-5 effort=nil"},
		{"unrelated home is never read", true, unavailableUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"PATH": os.Getenv("PATH")}
			if tc.decoy {
				env["SWARM_TELEMETRY_DECOY"] = decoy
			}
			runner := &CLIAdapter{agentName: "claude", driver: "claude", binary: os.Args[0], model: "configured-model",
				prefixArgs: []string{"-test.run=^TestTelemetrySessionHelper$", "--", "--swarm-telemetry-helper"},
				envAllow:   []string{"SWARM_TELEMETRY_DECOY"}, lookupEnv: mapLookup(env)}
			req := RunRequest{Role: "developer", WorktreePath: t.TempDir(), TaskPrompt: "task", Model: "configured-model"}
			_, wantArgs := runner.BuildCommand(req)
			res, err := runner.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != "helper stdout\n" || !slices.Equal(res.Command[1:], wantArgs) {
				t.Fatalf("stdout or argv changed: %q %q", res.Output, res.Command)
			}
			tel := res.Telemetry
			if tel == nil || tel.Validate() != nil {
				t.Fatalf("invalid telemetry: %+v", tel)
			}
			got := usageSummary(sessionUsage{tel.InputTokens, tel.OutputTokens, tel.CachedInputTokens, tel.ToolCalls, tel.RepeatedCommands, tel.ActualModel, tel.Effort})
			if got != tc.want {
				t.Fatalf("usage = %s, want %s", got, tc.want)
			}
			// The configured model is provenance; it is never reported as observed.
			if res.Attestation.Model != "configured-model" || strValue(tel.ActualModel) == "configured-model" {
				t.Fatalf("configured model leaked into observed model: %+v", tel)
			}
			if tel.ElapsedMS == nil || tel.SetupMS == nil || tel.PreflightMS == nil || tel.SnapshotMS != nil || tel.ModelMS != nil || tel.ToolMS != nil {
				t.Fatalf("lifecycle scopes = %+v", tel)
			}
			encoded, err := tel.Encode()
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{"ls", "helper stdout", decoy, req.WorktreePath, "task"} {
				if strings.Contains(encoded, `"`+leak+`"`) || strings.Contains(encoded, leak+"/") {
					t.Fatalf("telemetry persisted raw data %q: %s", leak, encoded)
				}
			}
		})
	}
}

func TestTelemetryMockRunnerIsDeterministic(t *testing.T) {
	res, err := (&MockRunner{}).Run(context.Background(), RunRequest{Role: "tester", WorktreePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tel := res.Telemetry
	if tel == nil || tel.Validate() != nil || tel.UsageSource != contract.UsageSourceMock || tel.Role != "tester" {
		t.Fatalf("mock telemetry = %+v", tel)
	}
	got := usageSummary(sessionUsage{tel.InputTokens, tel.OutputTokens, tel.CachedInputTokens, tel.ToolCalls, tel.RepeatedCommands, tel.ActualModel, tel.Effort})
	if got != "in=1200 out=300 cached=0 calls=0 repeats=0 model=mock effort=nil" {
		t.Fatalf("mock usage = %s", got)
	}
	data, _ := json.Marshal(tel)
	if !strings.Contains(string(data), `"effort":null`) || !strings.Contains(string(data), `"cached_input_tokens":0`) {
		t.Fatalf("unavailable must be null and measured zero must stay zero: %s", data)
	}
}

func TestTelemetryRunReviewAddsOuterSnapshotOnce(t *testing.T) {
	root := t.TempDir()
	mustWriteReviewFixture(t, root, "tracked.txt", "original")
	res, err := RunReview(context.Background(), &MockRunner{}, RunRequest{Role: "reviewer", WorktreePath: root, Mode: "headless"})
	if err != nil {
		t.Fatal(err)
	}
	tel := res.Telemetry
	if tel == nil || tel.SnapshotMS == nil || tel.ElapsedMS == nil || *tel.ElapsedMS < *tel.SnapshotMS {
		t.Fatalf("reviewer snapshot telemetry = %+v", tel)
	}
	if got, err := os.ReadFile(filepath.Join(root, ".swarm", "active", "REVIEW.md")); err != nil || !strings.HasPrefix(string(got), "VERDICT: APPROVED") {
		t.Fatalf("review import changed: %q %v", got, err)
	}
}
