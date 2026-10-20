package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestAdapterCommandBuilders(t *testing.T) {
	sysFile := ".swarm/active/SYSTEM_PROMPT.md"
	task := "Execute /swarm-plan"

	tests := []struct {
		agent    string
		mode     string
		wantBin  string
		wantArgs []string
	}{
		{
			agent:    "claude",
			mode:     "headless",
			wantBin:  "claude",
			wantArgs: []string{"-p", "--append-system-prompt-file", sysFile, task},
		},
		{
			agent:    "claude",
			mode:     "interactive",
			wantBin:  "claude",
			wantArgs: []string{"--append-system-prompt-file", sysFile, task},
		},
		{
			agent:    "omp",
			mode:     "headless",
			wantBin:  "omp",
			wantArgs: []string{"-p", "--no-pty", "--append-system-prompt", sysFile, task},
		},
		{
			agent:    "omp",
			mode:     "interactive",
			wantBin:  "omp",
			wantArgs: []string{"--append-system-prompt", sysFile, task},
		},
		{
			agent:    "grok",
			mode:     "headless",
			wantBin:  "grok",
			wantArgs: []string{"-p", "Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md first.\n\nExecute /swarm-plan"},
		},
		{
			agent:    "grok",
			mode:     "interactive",
			wantBin:  "grok",
			wantArgs: []string{task},
		},
		{
			agent:    "codex",
			mode:     "headless",
			wantBin:  "codex",
			wantArgs: []string{"exec", "Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md first.\n\nExecute /swarm-plan"},
		},
		{
			agent:    "codex",
			mode:     "interactive",
			wantBin:  "codex",
			wantArgs: []string{task},
		},
		{
			agent:    "dot",
			mode:     "headless",
			wantBin:  "dot",
			wantArgs: []string{"exec", "Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md first.\n\nExecute /swarm-plan"},
		},
		{
			agent:    "dot",
			mode:     "interactive",
			wantBin:  "dot",
			wantArgs: []string{task},
		},
		{
			agent:    "opencode",
			mode:     "headless",
			wantBin:  "opencode",
			wantArgs: []string{"run", "Read .swarm/active/SYSTEM_PROMPT.md and .swarm/active/WORKUNIT.md first.\n\nExecute /swarm-plan"},
		},
		{
			agent:    "opencode",
			mode:     "interactive",
			wantBin:  "opencode",
			wantArgs: []string{"--prompt", task},
		},
	}

	for _, tc := range tests {
		runner, err := NewRunner(tc.agent)
		if err != nil {
			t.Fatalf("NewRunner(%q) failed: %v", tc.agent, err)
		}
		bin, args := runner.BuildCommand(RunRequest{
			Agent:        tc.agent,
			Role:         "planner",
			Mode:         tc.mode,
			SystemPrompt: sysFile,
			TaskPrompt:   task,
		})
		if bin != tc.wantBin {
			t.Errorf("%s (%s): expected bin %q, got %q", tc.agent, tc.mode, tc.wantBin, bin)
		}
		if !reflect.DeepEqual(args, tc.wantArgs) {
			t.Errorf("%s (%s): expected args %#v, got %#v", tc.agent, tc.mode, tc.wantArgs, args)
		}
		if tc.agent == "codex" || tc.agent == "dot" || tc.agent == "grok" || tc.agent == "opencode" {
			if tc.mode == "headless" && !strings.Contains(args[1], ".swarm/active/WORKUNIT.md") {
				t.Errorf("%s headless prompt missing WORKUNIT.md reference", tc.agent)
			}
		}
	}
}
