package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
)

const childSecret = "ghp_HOSTILE_CHILD_291"
const childPrompt = "PRIVATE_PROMPT_MARKER_291"
const childEnv = "PRIVATE_ENV_VALUE_291"

func TestProgressHostileChildHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "--hostile-progress-child") {
		return
	}
	fmt.Fprintln(os.Stdout, childSecret)
	fmt.Fprintln(os.Stderr, childPrompt)
	fmt.Fprintln(os.Stdout, os.Getenv("PROGRESS_PRIVATE_VALUE"))
	os.Exit(0)
}

// This local mock still owns artifact generation and reviewer snapshot behavior.
// The extra CLI adapter runs a child that only writes stdout/stderr, exercising
// the real capture/observation path without needing a model or nested sandbox.
type hostileOutputRunner struct {
	*agent.MockRunner
	child    agent.Runner
	captured chan string
}

func (r *hostileOutputRunner) Run(ctx context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	childReq := agent.RunRequest{Role: req.Role, WorktreePath: req.WorktreePath, TaskPrompt: childPrompt}
	got, err := r.child.Run(ctx, childReq)
	if err != nil {
		return nil, err
	}
	select {
	case r.captured <- got.Output:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result, err := r.MockRunner.Run(ctx, req)
	if result != nil {
		result.Output = got.Output
	}
	return result, err
}
func TestProgressContainsOnlyMetadataChildOutput(t *testing.T) {
	for _, path := range []string{"work", "review", "submit"} {
		t.Run(path, func(t *testing.T) {
			f := newCLIFixture(t)
			// A real validation child emits the same private values on both streams.
			policy := "required_checks:\n  - name: safe-check\n    command: PROGRESS_PRIVATE_VALUE=" + childEnv + "; export PROGRESS_PRIVATE_VALUE; printf '" + childSecret + "\\n" + childPrompt + "\\n'; printf '%s\\n' \"$PROGRESS_PRIVATE_VALUE\" >&2\n    timeout: 1m\nprotected_paths: ['.swarm/**', '.github/**']\n"
			if err := os.WriteFile(filepath.Join(f.root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			prGit(t, f.root, "add", ".")
			prGit(t, f.root, "commit", "-m", "private-output validation fixture")
			n := strconv.Itoa(f.create(t, "agent"))
			f.must(t, "issue", "approve", n)
			args := []string{"issue", "work", n, "--agent", "mock", "--phase", "plan", "--json"}
			if path == "review" {
				f.must(t, "issue", "work", n, "--agent", "mock", "--phase", "plan", "--progress=false")
				args = []string{"issue", "review-plan", n, "--agent", "mock", "--json"}
			}
			if path == "submit" {
				f.must(t, "issue", "work", n, "--agent", "mock", "--progress=false")
				args = []string{"issue", "submit", n, "--json"}
			}
			captures := make(chan string, 10)
			if path != "submit" {
				childCfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"hostile": {Binary: os.Args[0], Harness: "claude", Args: []string{"-test.run=^TestProgressHostileChildHelper$", "--", "--hostile-progress-child"}, Env: []string{"PROGRESS_PRIVATE_VALUE"}}}}
				child, err := agent.NewConfiguredRunnerWithOptions("hostile", childCfg, true, agent.RunnerOptions{LookupEnv: func(k string) (string, bool) {
					switch k {
					case "SWARM_EXECUTION_MODE":
						return "trusted-local", true
					case "PROGRESS_PRIVATE_VALUE":
						return childEnv, true
					}
					return os.LookupEnv(k)
				}})
				if err != nil {
					t.Fatal(err)
				}
				f.resolverRunner = func(string, *contract.SwarmConfig, bool) (agent.Runner, error) {
					return &hostileOutputRunner{MockRunner: &agent.MockRunner{}, child: child, captured: captures}, nil
				}
			}
			cmd := f.command(nil)
			var out, diag bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&diag)
			cmd.SetArgs(append([]string{"--root", f.root, "--backend", "local"}, args...))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatalf("%v: %s", err, diag.String())
			}
			if !strings.Contains(diag.String(), " started") {
				t.Fatal("progress was disabled")
			}
			captured := out.String()
			if path != "submit" {
				select {
				case captured = <-captures:
				case <-time.After(5 * time.Second):
					t.Fatal("no child capture")
				}
			}
			for _, private := range []string{childSecret, childPrompt, childEnv} {
				if !strings.Contains(captured, private) {
					t.Fatalf("capture lost %s: %s", private, captured)
				}
				if strings.Contains(diag.String(), private) {
					t.Fatalf("private child output on progress stderr: %s", diag.String())
				}
			}
		})
	}
}
