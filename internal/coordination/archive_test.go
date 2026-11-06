package coordination

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Git elides unchanged ref updates before sending the transaction. An explicit
// lease on such an update cannot fence initial state creation against archiving.
func TestArchiveUnchangedIndexLeaseProbe(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f := newRetentionFixture(t)
			f.git(f.clone, "remote", "set-url", "origin", "file://"+f.remote)
			index := "refs/heads/swarm-archive"
			if layout == HiddenLayout {
				index = "refs/swarm/archive"
			}
			first := f.git(f.clone, "rev-parse", "HEAD")
			f.git(f.clone, "commit", "--allow-empty", "-m", "index advanced")
			advanced := f.git(f.clone, "rev-parse", "HEAD")
			f.git(f.clone, "push", "origin", advanced+":"+index)
			// Request an unchanged update while supplying a stale expected index tip.
			// This succeeds: there is no server-side index lease in the transaction.
			if err := f.store.push(context.Background(), "push", "--atomic", "origin", "--force-with-lease="+index+":"+first, "--force-with-lease="+layout.StateRef(363)+":", advanced+":"+index, first+":"+layout.StateRef(363)); err != nil {
				t.Fatal(err)
			}
			if got := f.git(f.remote, "rev-parse", layout.StateRef(363)); got != first {
				t.Fatal("state creation missing")
			}
		})
	}
}

// A changed, parented index update is sent to receive-pack. Move the index in
// pre-receive, after Git has advertised it, to prove server-side rejection.
func TestArchiveParentedIndexLeaseProbe(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f := newRetentionFixture(t)
			f.git(f.clone, "remote", "set-url", "origin", "file://"+f.remote)
			index := "refs/heads/swarm-archive"
			if layout == HiddenLayout {
				index = "refs/swarm/archive"
			}
			first := f.git(f.clone, "rev-parse", "HEAD")
			f.git(f.clone, "push", "origin", first+":"+index)
			f.git(f.clone, "commit", "--allow-empty", "-m", "concurrent archive index")
			advanced := f.git(f.clone, "rev-parse", "HEAD")
			// The competing object must already be in the remote object database.
			f.git(f.clone, "push", "origin", "main")
			f.git(f.clone, "checkout", "--detach", first)
			f.git(f.clone, "commit", "--allow-empty", "-m", "admission generation")
			candidate := f.git(f.clone, "rev-parse", "HEAD")
			hook := fmt.Sprintf("#!/bin/sh\nunset GIT_QUARANTINE_PATH\ngit update-ref %s %s %s || exit 1\n", index, advanced, first)
			if err := os.WriteFile(filepath.Join(f.remote, "hooks", "pre-receive"), []byte(hook), 0700); err != nil {
				t.Fatal(err)
			}
			err := f.store.push(context.Background(), "push", "--atomic", "origin", "--force-with-lease="+index+":"+first, "--force-with-lease="+layout.StateRef(363)+":", candidate+":"+index, first+":"+layout.StateRef(363))
			if err == nil {
				t.Fatal("server accepted stale index lease")
			}
			if got := f.git(f.remote, "rev-parse", index); got != advanced {
				t.Fatal("competing index update missing")
			}
			if got := f.git(f.remote, "for-each-ref", "--format=%(objectname)", layout.StateRef(363)); got != "" {
				t.Fatal("atomic rejection created state")
			}
			if got := f.git(f.remote, "rev-parse", "refs/heads/main"); got != advanced {
				t.Fatal("unrelated ref changed")
			}
		})
	}
}
