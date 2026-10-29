package cli

import (
	"bytes"
	"context"
	"github.com/supernovae/swarmos/internal/fork"
	"github.com/supernovae/swarmos/internal/lease"
	"strconv"
	"strings"
	"testing"
)

func TestForkRequestStatusHelpAndArguments(t *testing.T) {
	for _, args := range [][]string{
		{"fork-coordinator", "request-status", "--help"},
		{"--fork", "fork-coordinator", "request-status"},
		{"fork-coordinator", "request-status", "1", "100"},
		{"--fork", "fork-coordinator", "request-status", "0", "100"},
		{"--fork", "fork-coordinator", "request-status", "1", "9223372036854775808"},
		{"--fork", "fork-coordinator", "request-status", "1", "0"},
		{"--fork", "--backend", "local", "fork-coordinator", "request-status", "1", "100"},
		{"--fork", "fork-coordinator", "request-status", "1", "100", "--event", "x"},
		{"fork-coordinator"},
		{"fork-coordinator", "--event", "missing-event"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newRootCmdWithOptions(&RootOptions{})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			err := cmd.Execute()
			if args[len(args)-1] == "--help" {
				if err != nil || !strings.Contains(out.String(), "request-status <issue> <request-id>") || strings.Contains(out.String(), "--event") || strings.Contains(out.String(), "--json") {
					t.Fatal(err, out.String())
				}
			} else if err == nil {
				t.Fatal("expected error", out.String())
			}
		})
	}
}

func TestForkRequestStatusStateOnly(t *testing.T) {
	f := newUpstreamRemoteFixture(t)
	root := f.aliceClone("request-status")
	body, err := fork.FormatRequestComment(lease.ForkRequest{Operation: "acquire", IssueNumber: 42, ForkRepo: "alice/swarm"})
	if err != nil {
		t.Fatal(err)
	}
	tr := f.githubTracker("octo/swarm", "alice", "User")
	c, err := tr.CreateComment(context.Background(), 42, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.dispatch(context.Background(), 42, c); err != nil {
		t.Fatal(err)
	}
	// Remove the response to exercise truthful state-only confirmation.
	f.mu.Lock()
	for _, comment := range f.comments {
		if strings.Contains(comment.Body, fork.ResultMarker) {
			comment.Body = "Coordinator response unavailable"
		}
	}
	f.mu.Unlock()
	before, posted := f.refs(), f.postedComments()
	opts := f.as("alice", root)
	for range 2 {
		out, err := f.run(opts, "--fork", "--upstream-remote", "upstream", "fork-coordinator", "request-status", "42", strconv.FormatInt(c.ID, 10))
		if err != nil || !strings.Contains(out, "(acquire) on issue #42 confirmed by upstream state") || !strings.Contains(out, "does not resume work, restore a local claim token") || strings.Contains(out, "posted") {
			t.Fatal(err, out)
		}
	}
	if f.refs() != before || f.postedComments() != posted {
		t.Fatal("recovery mutated upstream")
	}
}
