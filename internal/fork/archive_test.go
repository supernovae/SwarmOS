package fork

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
)

func TestArchivedCoordinatorAdmission(t *testing.T) {
	for _, layout := range []coordination.RefLayout{coordination.BranchLayout, coordination.HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			root := t.TempDir()
			remote := filepath.Join(root, "source.git")
			clone := filepath.Join(root, "clone")
			destination := filepath.Join(root, "archive.git")
			git := func(dir string, args ...string) string {
				t.Helper()
				out, err := gitfixture.Command(t, "", append([]string{"-C", dir}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("git: %s %v", out, err)
				}
				return strings.TrimSpace(string(out))
			}
			git(root, "init", "--bare", "--initial-branch=main", remote)
			git(root, "clone", remote, clone)
			git(clone, "config", "user.name", "Test")
			git(clone, "config", "user.email", "test@example.invalid")
			if _, err := harness.InitRepository(harness.InitOptions{RepoRoot: clone}); err != nil {
				t.Fatal(err)
			}
			setLedgerNamespace(t, clone, layout.Name())
			git(clone, "add", ".")
			git(clone, "commit", "-m", "config")
			git(clone, "push", "origin", "main")
			git(root, "init", "--bare", destination)
			git(clone, "remote", "set-url", "origin", "file://"+remote)
			store := coordination.NewGitStoreWithLayout(clone, "origin", layout)
			ctx := context.Background()
			tip, err := store.CompareAndSwap(ctx, 363, "", []byte(`{"lease":{"issue_number":363,"status":"completed"}}`))
			if err != nil {
				t.Fatal(err)
			}
			refs, _, err := store.ArchiveSnapshot(ctx, 363)
			if err != nil {
				t.Fatal(err)
			}
			manifest := &contract.ArchiveManifest{Version: 1, Repository: "octo/swarm", Issue: 363, Namespace: layout.Name(), Destination: "file://" + destination, Refs: refs, StateTip: tip, PROutcome: "none", Actor: "maintainer", ArchivedAt: time.Now().UTC()}
			if err = store.Archive(ctx, manifest, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			coord := &Coordinator{RepoRoot: clone, UpstreamRepo: "octo/swarm"}
			_, _, err = coord.executeRequest(ctx, 500, "alice", lease.ForkRequest{Version: 1, IssueNumber: 363, Operation: "acquire"})
			var archived *coordination.ArchivedError
			if !errors.As(err, &archived) {
				t.Fatalf("archive error replaced: %v", err)
			}
		})
	}
}
