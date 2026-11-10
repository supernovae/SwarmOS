package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/testutil"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/workflow"
	"gopkg.in/yaml.v3"
)

func TestDryRunReportsIsolationBlockersWithoutClaim(t *testing.T) {
	t.Parallel()
	f := newCLIFixture(t)
	// Exercise a missing harness even when the developer has Codex installed.
	cfg, err := harness.LoadConfig(f.root)
	if err != nil {
		t.Fatal(err)
	}
	ac := cfg.Agents["codex"]
	ac.Binary = filepath.Join(t.TempDir(), "missing-codex")
	cfg.Agents["codex"] = ac
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", ".swarm/config.yaml"}, {"commit", "-m", "configure unavailable test harness"}} {
		if out, err := gitfixture.Command(t, "", append([]string{"-C", f.root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	f.setenv("SWARM_EXECUTION_MODE", "")
	var result struct {
		Plan workflow.PreflightResult `json:"effective_plan"`
	}
	if err := json.Unmarshal([]byte(f.must(t, "issue", "work", n, "--dry-run")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Plan.Execution.Mode != contract.ExecutionIsolated || result.Plan.Execution.Backend != contract.BackendNative || len(result.Plan.Blockers) == 0 {
		t.Fatalf("missing default boundary diagnostics: %+v", result.Plan)
	}
	issue, _ := strconv.Atoi(n)
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("inspection acquired a lease")
	}
	if out, err := f.run("issue", "work", n); err == nil || !strings.Contains(out+err.Error(), "preflight") {
		t.Fatalf("work did not fail closed: %v %s", err, out)
	}
	if st := f.state(t, issue); st.Lease.Status == "active" || st.Lease.ClaimID != "" {
		t.Fatal("blocked execution acquired a lease")
	}
}

func TestInspectionCommandSessionPerExecution(t *testing.T) {
	opts := &RootOptions{RepoRoot: t.TempDir(), lookupEnv: func(name string) (string, bool) {
		if name == "SWARM_NODE_CONFIG" {
			return filepath.Join(t.TempDir(), "node.yaml"), true
		}
		return "", false
	}}
	root := newRootCmdWithOptions(opts)
	var seen []*sandbox.InspectionSession
	var lookupSeen []*gitcredential.LookupSession
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, args []string) error {
		seen = append(seen, sandbox.InspectionSessionFrom(cmd.Context()))
		lookupSeen = append(lookupSeen, gitcredential.LookupSessionFrom(cmd.Context()))
		return nil
	}})
	for range 2 {
		root.SetArgs([]string{"probe"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] == nil || seen[1] == nil || seen[0] == seen[1] {
		t.Fatalf("commands did not each receive a fresh inspection session: %v", seen)
	}
	if len(lookupSeen) != 2 || lookupSeen[0] == nil || lookupSeen[1] == nil || lookupSeen[0] == lookupSeen[1] {
		t.Fatal("lookup session not reset")
	}
}

func TestToolCapabilityOptionalFailureDryRun(t *testing.T) {
	f := newCLIFixture(t)
	n := strconv.Itoa(f.create(t, "agent"))
	f.must(t, "issue", "approve", n)
	for _, format := range []string{"json", "text"} {
		var output bytes.Buffer
		calls := 0
		cmd := f.command(&RootOptions{inspectCapabilityEntry: func(id string) error {
			if id == "browser" {
				calls++
				return fmt.Errorf("TOKEN /home/private-user/secret")
			}
			return nil
		}})
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--root", f.root, "issue", "work", n, "--dry-run", "--agent", "mock", "--format=" + format})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err, output.String())
		}
		if calls == 0 {
			t.Fatal("report assembler was not exercised")
		}
		if strings.Contains(output.String(), "TOKEN") || strings.Contains(output.String(), "/home/private-user") {
			t.Fatal("secret escaped")
		}
		if format == "json" {
			var result struct {
				Plan workflow.PreflightResult `json:"effective_plan"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Plan.Blockers) > 0 {
				t.Fatal(result.Plan.Blockers)
			}
			for _, r := range result.Plan.ToolCapabilities {
				if r.Entries[1].Runtime != "not_probed" {
					t.Fatal(r)
				}
			}
		} else if !strings.Contains(output.String(), "runtime=not_probed") {
			t.Fatal(output.String())
		}
	}
	issue, _ := strconv.Atoi(n)
	if st := f.state(t, issue); st.Lease.ClaimID != "" {
		t.Fatal("dry-run acquired a claim")
	}
	_, err := f.run("issue", "work", n, "--dry-run", "--format=text", "--json")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatal(err)
	}
}

func TestImmutableLookupCommandLifecycle(t *testing.T) {
	f := newCLIFixture(t)
	n := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(n))
	st := f.state(t, n)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "reads")
	script := `#!/bin/sh
case "$1" in
show) printf '%s\n' "$*" >> '` + log + `' ;;
rev-parse) for arg; do if [ "$arg" = --git-common-dir ]; then printf 'COMMON\n' >> '` + log + `'; fi; done ;;
esac
exec '` + realGit + `' "$@"
`
	if err := testutil.WriteExecutable(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	opts := f.options(&RootOptions{})
	root := newRootCmdWithOptions(opts)
	var sessions []*gitcredential.LookupSession
	root.AddCommand(&cobra.Command{Use: "lookup-probe", RunE: func(cmd *cobra.Command, args []string) error {
		s := gitcredential.LookupSessionFrom(cmd.Context())
		if s == nil {
			t.Fatal("missing command lookup session")
		}
		sessions = append(sessions, s)
		_, manager, _, err := opts.resolveLeaseManager(cmd)
		if err != nil {
			return err
		}
		if manager.LookupSession() != s {
			t.Fatal("manager did not receive command session")
		}
		for range 2 {
			if _, err := harness.LoadConfigAt(f.root, st.BaseSHA, s); err != nil {
				return err
			}
			if _, err := harness.LoadValidationSpecAt(f.root, st.BaseSHA, s); err != nil {
				return err
			}
			budgetState := *st
			budgetState.Lease.ClaimedAt = time.Now()
			budgetState.Lease.ExpiresAt = time.Now().Add(time.Hour)
			if _, err := manager.Budget(&budgetState); err != nil {
				return err
			}
		}
		beforeScope, _ := os.ReadFile(log)
		siblingCtx := gitcredential.WithLookupSession(context.Background(), s)
		_, a, err := storageuse.Ensure(siblingCtx, f.root)
		if err != nil {
			return err
		}
		_, b, err := storageuse.Ensure(siblingCtx, f.root)
		if err != nil {
			return err
		}
		if a == b || a.Repository() != b.Repository() {
			t.Fatal("storage scopes incorrectly shared ownership")
		}
		afterScope, _ := os.ReadFile(log)
		if strings.Count(string(afterScope), "COMMON")-strings.Count(string(beforeScope), "COMMON") != 1 {
			t.Fatal("sibling scopes did not share one resolution")
		}
		return nil
	}})
	for i := 1; i <= 2; i++ {
		root.SetArgs([]string{"--root", f.root, "lookup-probe"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, key := range []string{st.BaseSHA + ":.swarm/config.yaml", st.BaseSHA + ":.swarm/validation/required-checks.yaml"} {
			if got := strings.Count(text, key); got != i {
				t.Fatalf("execution %d: %s observed %d times; log=%s", i, key, got, text)
			}
		}
	}
	if sessions[0] == sessions[1] {
		t.Fatal("commands share lookup state")
	}
}
