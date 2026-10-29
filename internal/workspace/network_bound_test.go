package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/testutil/gittransport"
)

func TestPublishPRLandedInterruptionAndSuccessor(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(map[bool]string{false: "landed", true: "successor"}[successor], func(t *testing.T) {
			repo, remote, base, expected := prFixture(t)
			path, branch, err := PreparePR(context.Background(), repo, PRWorkspaceOptions{Number: 42, Remote: "origin", HeadBranch: "feature", HeadSHA: expected, BaseSHA: base})
			if err != nil {
				t.Fatal(err)
			}
			runWorkspaceGit(t, path, "commit", "--allow-empty", "-m", "candidate")
			candidate := workspaceGitOutput(t, path, "rev-parse", "HEAD")
			tr := gittransport.New(t, remote)
			tr.Configure(t, repo, "origin")
			tr.Set("receive", "land-hang")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			tr.Watch(15*time.Second, func() { err = PublishPR(ctx, repo, "origin", "feature", expected, candidate) })
			if time.Since(start) > 15*time.Second {
				t.Error("unbounded publication")
			}
			if !errors.Is(err, errGitFailed) || !errors.Is(err, gitcredential.ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, gitcredential.ErrCleanupIncomplete) {
				t.Errorf("lost error chain: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "publish PR head with exact remote lease") {
				t.Errorf("lost prefix: %v", err)
			}
			if !tr.Landed() {
				t.Error("push did not land")
			}
			tr.Release()
			tr.Join()
			tr.Set("receive", "pass")
			observed, err := gitOutputContext(context.Background(), repo, "ls-remote", "origin", "refs/heads/feature")
			if err != nil || !strings.HasPrefix(observed, candidate) {
				t.Fatalf("authoritative head: %q %v", observed, err)
			}
			winner := candidate
			if successor {
				// A tracked independent client writes S using the local bare destination.
				other := t.TempDir()
				runWorkspaceGit(t, other, "clone", remote, ".")
				runWorkspaceGit(t, other, "checkout", "feature")
				runWorkspaceGit(t, other, "commit", "--allow-empty", "-m", "successor")
				winner = workspaceGitOutput(t, other, "rev-parse", "HEAD")
				runWorkspaceGit(t, other, "push", "origin", "feature")
				observed, err = gitOutputContext(context.Background(), repo, "ls-remote", "origin", "refs/heads/feature")
				if err != nil || !strings.HasPrefix(observed, winner) {
					t.Fatalf("successor observation: %q %v", observed, err)
				}
			}
			receives := tr.Receives()
			if err = PublishPR(context.Background(), repo, "origin", "feature", expected, candidate); err == nil {
				t.Error("stale captured head accepted")
			}
			if tr.Receives() != receives {
				t.Error("retry pushed before checking exact head")
			}
			if got := workspaceGitOutput(t, remote, "rev-parse", "refs/heads/feature"); got != winner {
				t.Error("authoritative head overwritten")
			}
			if got := workspaceGitOutput(t, repo, "rev-parse", branch); got != candidate {
				t.Error("candidate branch lost")
			}
			if _, err = os.Stat(filepath.Join(path, ".git")); err != nil {
				t.Error("candidate worktree lost")
			}
			if got := workspaceGitOutput(t, path, "rev-parse", "HEAD"); got != candidate {
				t.Error("candidate commit lost")
			}
		})
	}
}

func TestPushForkCheckpointInterruptedAndKnownLanded(t *testing.T) {
	for _, mode := range []string{"hang", "holder", "reject", "holder-reject"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newUpstreamRemoteFixture(t)
			branch := "swarm/issue-288-network"
			runWorkspaceGit(t, f.root, "checkout", "-b", branch)
			tr := gittransport.New(t, f.fork)
			tr.Configure(t, f.root, "origin")
			tr.Set("receive", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			tr.Watch(15*time.Second, func() { err = PushForkCheckpointFrom(ctx, f.root, "upstream", "origin", branch, f.forkBase) })
			if !errors.Is(err, errGitFailed) {
				t.Errorf("workspace error lost: %v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "push checkpoint to fork remote") {
				t.Errorf("prefix: %v", err)
			}
			switch mode {
			case "hang":
				if !errors.Is(err, gitcredential.ErrInterrupted) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, gitcredential.ErrCleanupIncomplete) {
					t.Errorf("interrupted chain: %v", err)
				}
			case "holder":
				var known *gitcredential.KnownResultCleanupError
				if !errors.As(err, &known) || known.ExitCode != 0 || errors.Is(err, gitcredential.ErrInterrupted) {
					t.Errorf("known landed chain: %v", err)
				}
			case "reject", "holder-reject":
				var exit *exec.ExitError
				if !errors.As(err, &exit) || errors.Is(err, gitcredential.ErrInterrupted) {
					t.Errorf("ordinary failure: %v", err)
				}
			}
			if got := workspaceGitOutput(t, f.root, "rev-parse", branch); got != f.forkBase {
				t.Error("candidate branch removed")
			}
		})
	}
}
func TestPublishPRKnownLandedCleanup(t *testing.T) {
	repo, remote, _, expected := prFixture(t)
	runWorkspaceGit(t, repo, "commit", "--allow-empty", "-m", "candidate")
	candidate := workspaceGitOutput(t, repo, "rev-parse", "HEAD")
	tr := gittransport.New(t, remote)
	tr.Configure(t, repo, "origin")
	tr.Set("receive", "holder")
	var err error
	tr.Watch(10*time.Second, func() { err = PublishPR(context.Background(), repo, "origin", "feature", expected, candidate) })
	var known *gitcredential.KnownResultCleanupError
	if !errors.Is(err, errGitFailed) || !errors.As(err, &known) || known.ExitCode != 0 || errors.Is(err, gitcredential.ErrInterrupted) {
		t.Errorf("known landed status: %v", err)
	}
	if got := workspaceGitOutput(t, remote, "rev-parse", "refs/heads/feature"); got != candidate {
		t.Error("known landed head not present")
	}
	if tr.Receives() != 1 {
		t.Error("known landed push was retried")
	}
}
func TestGitOutputNetworkErrorsAndRedaction(t *testing.T) {
	// Token resolution is cached for a coordinator process. Start a fresh test
	// process with the fixture credential before any Git helper can resolve it.
	if os.Getenv("SWARM_TEST_NETWORK_REDACTION") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGitOutputNetworkErrorsAndRedaction$", "-test.v")
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			switch key {
			case "SWARM_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", gitcredential.TokenCommandEnv:
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		cmd.Env = append(cmd.Env, "SWARM_TEST_NETWORK_REDACTION=1", "SWARM_GITHUB_TOKEN=secret288")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("redaction subprocess: %v\n%s", err, out)
		}
		return
	}
	for _, mode := range []string{"hang", "reject", "holder"} {
		t.Run(mode, func(t *testing.T) {
			repo, remote, _, _ := prFixture(t)
			tr := gittransport.New(t, remote)
			tr.Configure(t, repo, "origin")
			tr.Set("upload", mode)
			script := filepath.Join(tr.Dir, "ssh")
			b, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			b = []byte(strings.ReplaceAll(string(b), "fixture rejected", "secret288 "+base64.StdEncoding.EncodeToString([]byte("x-access-token:secret288"))))
			if err = os.WriteFile(script, b, 0700); err != nil {
				t.Fatal(err)
			}
			runWorkspaceGit(t, repo, "remote", "set-url", "origin", "ssh://secret288@fake.invalid"+remote)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out string
			tr.Watch(15*time.Second, func() { out, err = gitOutputContext(ctx, repo, "ls-remote", "origin") })
			if out != "" || !errors.Is(err, errGitFailed) {
				t.Errorf("accepted incomplete observation: %q %v", out, err)
			}
			if strings.Contains(err.Error(), "secret288") || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("x-access-token:secret288"))) {
				t.Errorf("credential escaped redaction: %v", err)
			}
			if mode == "reject" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Errorf("exit cause lost: %v", err)
				}
			}
			if mode == "hang" && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("context lost: %v", err)
			}
		})
	}
	// A displayed cause may be redacted without discarding typed identity.
	secretCause := errors.New("secret288")
	wrapped := &gitOutputError{message: "git command failed: [REDACTED]", cause: secretCause}
	if !errors.Is(wrapped, secretCause) || !errors.Is(wrapped, errGitFailed) || strings.Contains(wrapped.Error(), "secret288") {
		t.Error("dual wrapped cause lost")
	}
}
