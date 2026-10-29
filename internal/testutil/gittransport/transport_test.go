package gittransport

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
)

func TestTransportModes(t *testing.T) {
	for _, mode := range []string{"pass", "reject", "hang", "land-hang", "holder"} {
		t.Run(mode, func(t *testing.T) {
			repo := gitfixture.Init(t, "")
			bare := gitfixture.Init(t, "", "--bare")
			gitfixture.Run(t, repo, "commit", "--allow-empty", "-m", "candidate")
			head := strings.TrimSpace(gitfixture.Run(t, repo, "rev-parse", "HEAD"))
			gitfixture.Run(t, repo, "remote", "add", "origin", bare)
			tr := New(t, bare)
			tr.Configure(t, repo, "origin")
			tr.Set("receive", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var err error
			tr.Watch(15*time.Second, func() { _, err = gitcredential.Output(ctx, repo, "push", "origin", "HEAD:refs/heads/candidate") })
			switch mode {
			case "pass":
				if err != nil {
					t.Errorf("pass: %v", err)
				}
			case "reject":
				if err == nil || errors.Is(err, gitcredential.ErrInterrupted) {
					t.Errorf("reject: %v", err)
				}
			case "hang", "land-hang":
				if !errors.Is(err, gitcredential.ErrInterrupted) {
					t.Errorf("hang: %v", err)
				}
			case "holder":
				var known *gitcredential.KnownResultCleanupError
				if !errors.As(err, &known) || known.ExitCode != 0 {
					t.Errorf("holder: %v", err)
				}
				if !Alive(tr.Master()) {
					t.Error("master was signaled")
				}
			}
			if mode == "land-hang" {
				if !tr.Landed() {
					t.Error("post-receive not reached")
				}
				if got := strings.TrimSpace(gitfixture.Run(t, bare, "rev-parse", "refs/heads/candidate")); got != head {
					t.Error("landed marker preceded ref update")
				}
			}
			tr.Release()
			tr.Join()
			if mode == "pass" {
				out, e := gitcredential.Output(context.Background(), repo, "ls-remote", "origin")
				if e != nil || !strings.Contains(string(out), head) {
					t.Errorf("A1 Output pass: %q %v", out, e)
				}
			}
		})
	}
}
func TestTransportTeardownPaths(t *testing.T) {
	for _, scenario := range []string{"early assertion", "watchdog", "blocked waiter"} {
		t.Run(scenario, func(t *testing.T) {
			repo := gitfixture.Init(t, "")
			bare := gitfixture.Init(t, "", "--bare")
			gitfixture.Run(t, repo, "remote", "add", "origin", bare)
			tr := New(t, bare)
			tr.Configure(t, repo, "origin")
			tr.Set("upload", "hang")
			reports := 0
			tr.report = func(string, ...any) { reports++ }
			if scenario == "early assertion" {
				tr.report("simulated early assertion")
			}
			// The watchdog releases a real legacy CombinedOutput, including its blocked
			// I/O wait, then joins it before recording the simulated failure.
			tr.Watch(150*time.Millisecond, func() {
				cmd, _, err := gitcredential.Command(context.Background(), repo, "ls-remote", "origin")
				if err == nil {
					_, _ = cmd.CombinedOutput()
				}
			})
			tr.Release()
			tr.Join()
			if reports == 0 {
				t.Error("watchdog path was not exercised")
			}
			if len(tr.Clients()) == 0 {
				t.Error("fixture did not start")
			}
			for _, p := range tr.Clients() {
				if Alive(p) {
					t.Error("released client still alive")
				}
			}
			select {
			case <-tr.supervisorDone:
			default:
				t.Error("supervisor worker did not exit")
			}
			// Descriptor counts are checked by the runner tests; the holder test also
			// verifies a completed Wait and master-exit acknowledgement here.
			if _, err := os.Stat(tr.Dir + "/release"); err != nil {
				t.Error("fixture not released")
			}
		})
	}
}
