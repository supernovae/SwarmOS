package coordination

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A ref created before push advertisement with the intended value can cause
// Git to elide that ref update, including its create-only lease.
func TestArchiveRestoreIdenticalRefBeforeAdvertisement(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		for _, kind := range []string{"state", "checkpoint"} {
			t.Run(layout.Name()+"/"+kind, func(t *testing.T) {
				f, m := archiveFixture(t, layout)
				ctx := context.Background()
				if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
				ref, sha := layout.StateRef(363), m.StateTip
				if kind == "checkpoint" {
					for _, r := range m.Refs {
						if r.Source != layout.StateRef(363) {
							ref, sha = r.Source, r.SHA
							break
						}
					}
					if ref == layout.StateRef(363) {
						t.Fatal("missing checkpoint fixture")
					}
				}
				real, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				script := `#!/bin/sh
is_push=false
for arg do
 [ "$arg" != push ] || is_push=true
done
if [ "$is_push" = true ]; then
 "$RESTORE_RACE_REAL" --git-dir="$RESTORE_RACE_REMOTE" update-ref "$RESTORE_RACE_REF" "$RESTORE_RACE_SHA" || exit $?
fi
exec "$RESTORE_RACE_REAL" "$@"
`
				if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RESTORE_RACE_REAL", real)
				t.Setenv("RESTORE_RACE_SOURCE", "file://"+f.remote)
				t.Setenv("RESTORE_RACE_REMOTE", f.remote)
				t.Setenv("RESTORE_RACE_REF", ref)
				t.Setenv("RESTORE_RACE_SHA", sha)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				_, restoreErr := f.store.RestoreArchive(ctx, 363, "octo/swarm", true)
				idx, _, readErr := f.store.archiveIndex(ctx, false)
				if readErr != nil {
					t.Fatal(readErr)
				}
				_, retained := idx.Entries[363]
				if restoreErr == nil || !retained {
					t.Fatalf("restore must reject concurrent identical %s and retain tombstone: error=%v tombstone_retained=%t", kind, restoreErr, retained)
				}
			})
		}
	}
}

func TestArchiveRestoreRejectsCounterpartIndex(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			tip := f.git(f.remote, "rev-parse", layout.ArchiveRef())
			f.git(f.clone, "push", "origin", tip+":"+layout.Counterpart().ArchiveRef())
			before := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			if _, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err == nil {
				t.Error("restore accepted conflicting archive index namespaces")
			}
			if after := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
				t.Error("restore mutated source despite namespace conflict")
			}
		})
	}
}
