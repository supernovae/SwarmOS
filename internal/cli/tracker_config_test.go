package cli

import (
	"bytes"
	"context"
	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/registry"
	"github.com/supernovae/swarmos/internal/tracker"
	"strconv"
	"strings"
	"testing"
)

func TestBackendFlagRemoved(t *testing.T) {
	for _, prefix := range [][]string{nil, {"repo", "doctor"}, {"issue", "status"}} {
		for _, flag := range [][]string{{"--backend", "local"}, {"--backend=github"}, {"--backend", "github", "--help"}, {"--help", "--backend=local"}} {
			cmd := NewRootCmd()
			cmd.SetArgs(append(append([]string{}, prefix...), flag...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "tracker") || !strings.Contains(err.Error(), ".swarm/config.yaml") {
				t.Fatalf("%v %v: %v", prefix, flag, err)
			}
		}
	}
	var inspect func(*cobra.Command)
	inspect = func(cmd *cobra.Command) {
		if cmd.Flags().Lookup("backend") != nil || cmd.PersistentFlags().Lookup("backend") != nil {
			t.Fatalf("registered on %s", cmd.CommandPath())
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		_ = cmd.Help()
		if strings.Contains(out.String(), "--backend") {
			t.Fatalf("help: %s", out.String())
		}
		for _, child := range cmd.Commands() {
			inspect(child)
		}
	}
	inspect(NewRootCmd())
	cmd := NewRootCmd()
	cmd.SetArgs([]string{"--unknown"})
	if err := cmd.Execute(); err == nil || strings.Contains(err.Error(), "tracker") {
		t.Fatalf("unrelated flag: %v", err)
	}
}

func TestTrackerConfigAuthority(t *testing.T) {
	f := newCLIFixture(t)
	opts := f.options(&RootOptions{RepoRoot: f.root})
	tr, err := opts.resolveTracker(f.command(nil))
	if err != nil || tr.BackendName() != "local" {
		t.Fatalf("local: %v %v", tr, err)
	}
	opts.Fork = true
	if _, err := opts.resolveTracker(f.command(nil)); err == nil || !strings.Contains(err.Error(), "tracker: local") {
		t.Fatalf("fork: %v", err)
	}
	setFixtureTracker(t, f.root, "invalid")
	opts.trackerOverride = f.tracker
	if _, err := opts.resolveTracker(f.command(nil)); err == nil || !strings.Contains(err.Error(), "tracker") {
		t.Fatalf("invalid: %v", err)
	}
}

func TestTrackerConfigOverridesRegistry(t *testing.T) {
	for _, configured := range []string{"local", "github"} {
		t.Run(configured, func(t *testing.T) {
			f := newCLIFixture(t)
			other := "local"
			if configured == "local" {
				other = "github"
			}
			setFixtureTracker(t, f.root, other)
			reg := registry.New(f.options(nil).registryPath())
			if _, err := reg.Add("fixture/repo", f.root, configured); err != nil {
				t.Fatal(err)
			}
			setFixtureTracker(t, f.root, configured)
			opts := f.options(&RootOptions{Repo: "fixture/repo", remoteSlugFunc: func(string, string) string { return "fixture/repo" }})
			called := false
			opts.githubTrackerFunc = func(string) (tracker.Tracker, error) { called = true; return f.tracker, nil }
			if _, err := opts.resolveTracker(f.command(nil)); err != nil {
				t.Fatal(err)
			}
			if called != (configured == "github") {
				t.Fatalf("config %s did not select correct tracker", configured)
			}
			targets, err := opts.resolveTargets(true)
			if err != nil || len(targets) != 1 || targets[0].Backend != configured {
				t.Fatalf("targets: %v %v", targets, err)
			}
		})
	}
}

func TestTrackerRecoveryCommandsOmitBackend(t *testing.T) {
	opts := &RootOptions{Repo: "owner/repo", RepoRoot: "/path with space"}
	for _, backend := range []string{"local", "github"} {
		command := leaseReminderRenewCommand(opts, backend, 42)
		if strings.Contains(command, "--backend") || !strings.Contains(command, "--root '/path with space'") {
			t.Fatal(command)
		}
		argv := (stopContext{opts: opts}).argv(false, "issue", "work", "42")
		if strings.Contains(strings.Join(argv, " "), "--backend") {
			t.Fatal(argv)
		}
	}
}

func TestTrackerProjectionAndDoctorOutput(t *testing.T) {
	f := newCLIFixture(t)
	number := f.create(t, "agent")
	f.must(t, "issue", "approve", strconv.Itoa(number))
	f.must(t, "issue", "claim", strconv.Itoa(number), "--agent", "mock")
	comments, err := f.tracker.ListComments(context.Background(), number)
	if err != nil || len(comments) == 0 {
		t.Fatalf("projection comments: %v %v", comments, err)
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, "--backend") {
			t.Fatal(comment.Body)
		}
	}
	output, _ := f.run("repo", "doctor", "--json")
	if !strings.Contains(output, "tracker=local") || strings.Contains(output, "--backend") {
		t.Fatal(output)
	}
}
