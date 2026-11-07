package coordination

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveRestorePublicationFailureLeavesNoReservations(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			// The first source transaction succeeds. The remote then refuses
			// further writes, including any attempted compensating transaction.
			hook := "#!/bin/sh\nprintf '#!/bin/sh\\nexit 1\\n' > hooks/pre-receive\nchmod 700 hooks/pre-receive\n"
			if err := os.WriteFile(filepath.Join(f.remote, "hooks", "post-receive"), []byte(hook), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err == nil {
				t.Fatal("restore accepted rejected publication")
			}
			idx, _, err := f.store.archiveIndex(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := idx.Entries[363]; !ok {
				t.Fatal("lost tombstone")
			}
			// Restore should be retryable after the remote becomes writable.
			if err := os.Remove(filepath.Join(f.remote, "hooks", "pre-receive")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.remote, "hooks", "post-receive")); err != nil {
				t.Fatal(err)
			}
			for _, r := range m.Refs {
				if got := f.git(f.remote, "for-each-ref", "--format=%(objectname)", r.Source); got != "" {
					t.Errorf("partial restore: source ref %s contains reservation instead of remaining absent (archived SHA equal: %t)", r.Source, got == r.SHA)
				}
			}
			if _, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err != nil {
				t.Errorf("restore cannot recover after remote write rejection is removed: %v", err)
			}
		})
	}
}
