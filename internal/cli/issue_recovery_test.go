package cli

import (
	"context"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/workspace"
)

func TestApprovalDroppedCheckpointOutput(t *testing.T) {
	t.Parallel()
	for _, jsonOutput := range []bool{false, true} {
		t.Run(strconv.FormatBool(jsonOutput), func(t *testing.T) {
			f := newCLIFixture(t)
			issue := f.create(t, "none")
			n := strconv.Itoa(issue)
			f.must(t, "issue", "approve", n)
			st, err := f.manager.Acquire(context.Background(), issue, "alice", "mock", "plan", time.Hour, 1)
			if err != nil {
				t.Fatal(err)
			}
			branch := workspace.BranchName(issue, "Approved task")
			if _, err := f.manager.RecordCheckpoint(context.Background(), issue, "alice", branch, st.BaseSHA); err != nil {
				t.Fatal(err)
			}
			if _, err := f.manager.Release(context.Background(), issue, "alice", "test"); err != nil {
				t.Fatal(err)
			}
			out, err := gitfixture.Command(t, "", "-C", f.root, "commit", "--allow-empty", "-m", "new base").CombinedOutput()
			if err != nil {
				t.Fatalf("commit: %v %s", err, out)
			}
			args := []string{"issue", "approve", n}
			if jsonOutput {
				args = append(args, "--json")
			}
			output := f.must(t, args...)
			if jsonOutput {
				var result struct {
					State lease.IssueState `json:"state"`
				}
				if err := json.Unmarshal([]byte(output), &result); err != nil {
					t.Fatal(err)
				}
				if result.State.DroppedCheckpoint == nil || result.State.HeadSHA != "" || result.State.Branch != "" {
					t.Fatal("JSON lacks retired identity")
				}
			} else if !strings.Contains(output, "Dropped empty checkpoint") {
				t.Fatalf("missing notice: %s", output)
			}
			if repeat := f.must(t, "issue", "approve", n); strings.Contains(repeat, "Dropped empty checkpoint") {
				t.Fatal("repeat approval reported another drop")
			}
		})
	}
}
