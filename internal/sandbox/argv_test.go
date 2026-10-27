package sandbox

import (
	"slices"
	"testing"
)

func TestIsolatedClaudeArgvAdmitsOnlyModelAndEffort(t *testing.T) {
	base := []string{"--restricted", "--strict-mcp-config", "--mcp-config", "/c/mcp.json", "--tools", "", "--disable-slash-commands", "--no-chrome", "--settings", "/c/settings.json", "--permission-mode", "dontAsk", "--permission-prompts", "none", "--no-session-persistence", "--print"}
	for _, tc := range []struct {
		model, effort string
		extra         []string
	}{
		{"", "", nil},
		{"m", "", []string{"--model", "m"}},
		{"", "high", []string{"--effort", "high"}},
		{"m", "max", []string{"--model", "m", "--effort", "max"}},
	} {
		got := claudeArgs("/c/mcp.json", "/c/settings.json", tc.model, tc.effort)
		if want := append(slices.Clone(base), tc.extra...); !slices.Equal(got, want) {
			t.Fatalf("model=%q effort=%q:\n got %q\nwant %q", tc.model, tc.effort, got, want)
		}
	}
}

func TestNativeCodexArgvAdmitsOnlyModelAndEffort(t *testing.T) {
	base := []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "-C", "/w"}
	for _, tc := range []struct {
		model, effort string
		extra         []string
	}{
		{"", "", nil},
		{"m", "", []string{"--model", "m"}},
		{"", "minimal", []string{"-c", `model_reasoning_effort="minimal"`}},
		{"m", "high", []string{"--model", "m", "-c", `model_reasoning_effort="high"`}},
	} {
		got := codexExecArgs("/w", tc.model, tc.effort)
		if want := append(slices.Clone(base), tc.extra...); !slices.Equal(got, want) {
			t.Fatalf("model=%q effort=%q:\n got %q\nwant %q", tc.model, tc.effort, got, want)
		}
	}
}
