package coordination

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

func archiveFixture(t *testing.T, layout RefLayout) (*retentionFixture, *contract.ArchiveManifest) {
	t.Helper()
	f := newRetentionFixture(t)
	f.store = NewGitStoreWithLayout(f.clone, "origin", layout)
	f.git(f.clone, "remote", "set-url", "origin", "file://"+f.remote)
	dest := filepath.Join(f.root, "archive.git")
	f.git(f.root, "init", "--bare", dest)
	head := f.git(f.clone, "rev-parse", "HEAD")
	if _, err := f.store.CompareAndSwapRetaining(context.Background(), 363, "", []byte(`{"lease":{"issue_number":363,"status":"completed"}}`), head); err != nil {
		t.Fatal(err)
	}
	refs, tip, err := f.store.ArchiveSnapshot(context.Background(), 363)
	if err != nil {
		t.Fatal(err)
	}
	return f, &contract.ArchiveManifest{Version: 1, Repository: "octo/swarm", Issue: 363, Namespace: layout.Name(), Destination: "file://" + dest, Refs: refs, StateTip: tip, PROutcome: "none", Actor: "maintainer", ArchivedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func TestArchiveRoundTrip(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			before := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			local := f.git(f.clone, "for-each-ref", "--format=%(refname) %(objectname)")
			if _, _, err := f.store.ArchiveSnapshot(ctx, 363); err != nil {
				t.Fatal(err)
			}
			if got := f.git(f.clone, "for-each-ref", "--format=%(refname) %(objectname)"); got != local {
				t.Fatal("preview wrote local refs")
			}
			if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			var archived *ArchivedError
			if _, _, err := f.store.Load(ctx, 363); !errors.As(err, &archived) {
				t.Fatalf("load: %v", err)
			}
			if _, err := f.store.CompareAndSwap(ctx, 363, "", []byte(`{}`)); !errors.As(err, &archived) {
				t.Fatalf("recreate: %v", err)
			}
			if _, err := f.store.CompareAndSwapRetaining(ctx, 363, m.StateTip, []byte(`{}`), m.Refs[0].SHA); !errors.As(err, &archived) {
				t.Fatalf("retain: %v", err)
			}
			tomb, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", false)
			if err != nil || tomb == nil {
				t.Fatalf("preview: %v", err)
			}
			if _, err = f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err != nil {
				t.Fatal(err)
			}
			for _, r := range m.Refs {
				if got := f.git(f.remote, "rev-parse", r.Source); got != r.SHA {
					t.Fatal("restored SHA differs")
				}
			}
			if _, _, err = f.store.Load(ctx, 363); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.RestoreArchive(ctx, 363, "octo/swarm", true); !errors.Is(err, ErrNotFound) {
				t.Fatalf("repeat restore: %v", err)
			}
			if got := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); got == before {
				t.Fatal("index did not retain history")
			}
			idx, _, err := f.store.archiveIndex(ctx, false)
			if err != nil || len(idx.Entries) != 0 {
				t.Fatalf("index: %v", err)
			}
		})
	}
}

// assertArchiveInventories checks every ref, including unrelated refs, so retry
// success cannot hide extra archive copies or leftover live retention refs.
func assertArchiveInventories(t *testing.T, f *retentionFixture, m *contract.ArchiveManifest, before string) {
	t.Helper()
	inventory := func(repo string) map[string]string {
		refs := map[string]string{}
		for _, line := range strings.Split(f.git(repo, "for-each-ref", "--format=%(refname) %(objectname)"), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				refs[fields[0]] = fields[1]
			}
		}
		return refs
	}
	source := map[string]string{}
	for _, line := range strings.Split(before, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			source[fields[0]] = fields[1]
		}
	}
	tomb, err := f.store.ArchiveTombstone(context.Background(), m.Issue, false)
	if err != nil || tomb == nil {
		t.Fatalf("retry tombstone: %v", err)
	}
	if err := f.store.verifyArchive(context.Background(), *tomb); err != nil {
		t.Fatal(err)
	}
	destination := map[string]string{archiveManifestRef(m.Issue): tomb.ManifestTip}
	for _, r := range m.Refs {
		delete(source, r.Source)
		destination[r.Destination] = r.SHA
	}
	source[f.store.layout.ArchiveRef()] = f.git(f.remote, "rev-parse", f.store.layout.ArchiveRef())
	if got := inventory(f.remote); !reflect.DeepEqual(got, source) {
		t.Fatalf("source inventory: got %v want %v", got, source)
	}
	if got := inventory(strings.TrimPrefix(m.Destination, "file://")); !reflect.DeepEqual(got, destination) {
		t.Fatalf("destination inventory: got %v want %v", got, destination)
	}
}

func TestArchiveFailuresPreserveSource(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		for _, failure := range []string{"destination", "state", "checkpoint", "index", "recheck", "corrupt-manifest", "restore-conflict"} {
			t.Run(layout.Name()+"/"+failure, func(t *testing.T) {
				f, m := archiveFixture(t, layout)
				ctx := context.Background()
				before := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
				recheck := func(context.Context) error { return nil }
				switch failure {
				case "destination":
					dest := strings.TrimPrefix(m.Destination, "file://")
					if err := os.WriteFile(filepath.Join(dest, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
						t.Fatal(err)
					}
				case "recheck":
					recheck = func(context.Context) error { return fmt.Errorf("eligibility changed") }
				case "state", "checkpoint", "index":
					recheck = func(context.Context) error {
						ref := layout.StateRef(363)
						if failure == "checkpoint" {
							for _, r := range m.Refs {
								if r.Source != layout.StateRef(363) {
									ref = r.Source
								}
							}
						}
						if failure == "index" {
							ref = layout.ArchiveRef()
						}
						current := f.git(f.remote, "rev-parse", ref)
						next := f.git(f.clone, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", f.git(f.clone, "rev-parse", current+"^{tree}"), "-p", current, "-m", "raced")
						f.git(f.clone, "push", "origin", "+"+next+":"+ref)
						before = f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
						return nil
					}
				case "corrupt-manifest", "restore-conflict":
					if err := f.store.Archive(ctx, m, recheck); err != nil {
						t.Fatal(err)
					}
					if failure == "corrupt-manifest" {
						dest := strings.TrimPrefix(m.Destination, "file://")
						f.git(f.clone, "push", dest, "+"+m.StateTip+":"+archiveManifestRef(363))
					} else {
						f.git(f.clone, "push", "origin", m.StateTip+":"+layout.StateRef(363))
					}
					before = f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
					if _, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err == nil {
						t.Fatal("restore accepted corruption/conflict")
					}
					if f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)") != before {
						t.Fatal("failed restore changed source")
					}
					return
				}
				if err := f.store.Archive(ctx, m, recheck); err == nil {
					t.Fatal("archive accepted failure")
				}
				if got := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); got != before {
					t.Fatalf("failed archive changed source\n%s\n%s", before, got)
				}
				// Undo only the injected concurrent write; retry the same immutable snapshot.
				dest := strings.TrimPrefix(m.Destination, "file://")
				if failure == "destination" {
					if err := os.Remove(filepath.Join(dest, "hooks", "pre-receive")); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "state" || failure == "checkpoint" {
					for _, r := range m.Refs {
						f.git(f.clone, "push", "origin", "+"+r.SHA+":"+r.Source)
					}
				}
				if failure == "index" {
					// ArchiveSnapshot created the original empty index before the race.
					raced := f.git(f.remote, "rev-parse", layout.ArchiveRef())
					original := f.git(f.clone, "rev-parse", raced+"^")
					f.git(f.clone, "push", "origin", "+"+original+":"+layout.ArchiveRef())
				}
				before = f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
				if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
					t.Fatalf("retry: %v", err)
				}
				assertArchiveInventories(t, f, m, before)
			})
		}
	}
}
func TestArchivedMalformedIndexAndSingleton(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			if !IsLedgerRef(layout.ArchiveRef()) || IsLedgerRef(layout.ArchiveRef()+"/child") {
				t.Fatal("singleton matching")
			}
			f.git(f.clone, "push", "origin", "+"+m.StateTip+":"+layout.ArchiveRef())
			if _, _, err := f.store.Load(ctx, 999); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid index treated as new: %v", err)
			}
			if _, err := f.store.CompareAndSwap(ctx, 999, "", []byte(`{}`)); err == nil {
				t.Fatal("invalid index admitted write")
			}
		})
	}
}

func TestArchiveReadBackAndRestoreRaces(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		for _, mode := range []string{"missing-copy", "wrong-copy", "restore-state", "restore-checkpoint", "restore-index"} {
			t.Run(layout.Name()+"/"+mode, func(t *testing.T) {
				f, m := archiveFixture(t, layout)
				ctx := context.Background()
				dest := strings.TrimPrefix(m.Destination, "file://")
				inventory := func() string { return f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)") }
				before := inventory()
				if mode == "missing-copy" || mode == "wrong-copy" {
					ref := archiveManifestRef(363)
					action := "git update-ref -d " + ref
					if mode == "wrong-copy" {
						action = "git update-ref " + ref + " " + m.StateTip
					}
					if err := os.WriteFile(filepath.Join(dest, "hooks", "post-receive"), []byte("#!/bin/sh\n"+action+"\n"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err == nil {
						t.Fatal("read-back corruption accepted")
					}
					if inventory() != before {
						t.Fatal("failed verification changed source")
					}
					if err := os.Remove(filepath.Join(dest, "hooks", "post-receive")); err != nil {
						t.Fatal(err)
					}
					// Remove the injected bad manifest; never overwrite an immutable conflicting copy.
					f.git(dest, "update-ref", "-d", ref)
					if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
						t.Fatalf("read-back retry: %v", err)
					}
					assertArchiveInventories(t, f, m, before)
					return
				}
				if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
				ref := layout.StateRef(363)
				sha := m.StateTip
				old := strings.Repeat("0", 40)
				if mode == "restore-checkpoint" {
					for _, r := range m.Refs {
						if r.Source != ref {
							ref = r.Source
							sha = r.SHA
							break
						}
					}
				}
				if mode == "restore-index" {
					ref = layout.ArchiveRef()
					old = f.git(f.remote, "rev-parse", ref)
					sha = f.git(f.clone, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", f.git(f.clone, "rev-parse", old+"^{tree}"), "-p", old, "-m", "index race")
					f.git(f.clone, "push", "origin", sha+":refs/heads/race-object")
				}
				hook := fmt.Sprintf("#!/bin/sh\nunset GIT_QUARANTINE_PATH\ngit update-ref %s %s %s || exit 1\n", ref, sha, old)
				if err := os.WriteFile(filepath.Join(f.remote, "hooks", "pre-receive"), []byte(hook), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err == nil {
					t.Fatal("restore race accepted")
				}
				for _, r := range m.Refs {
					if r.Source != ref && f.git(f.remote, "for-each-ref", "--format=%(objectname)", r.Source) != "" {
						t.Fatal("partially restored refs")
					}
				}
				idx, _, err := f.store.archiveIndex(ctx, false)
				if err != nil || len(idx.Entries) != 1 {
					t.Fatalf("restore lost tombstone: %v", err)
				}
			})
		}
	}
}
func TestArchiveChangedSnapshotRetryAndUnrelatedEntry(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			if err := f.store.Archive(ctx, m, func(context.Context) error { return errors.New("retry") }); err == nil {
				t.Fatal("recheck ignored")
			}
			next, err := f.store.CompareAndSwap(ctx, 363, m.StateTip, []byte(`{"lease":{"issue_number":363,"status":"completed"},"generation":2}`))
			if err != nil {
				t.Fatal(err)
			}
			refs, _, err := f.store.ArchiveSnapshot(ctx, 363)
			if err != nil {
				t.Fatal(err)
			}
			m.Refs = refs
			m.StateTip = next
			if err = f.store.Archive(ctx, m, func(context.Context) error { return nil }); err == nil {
				t.Fatal("changed snapshot overwrote archive")
			}
			dest := filepath.Join(f.root, "clean.git")
			f.git(f.root, "init", "--bare", dest)
			m.Destination = "file://" + dest
			if err = f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			tomb, err := f.store.ArchiveTombstone(ctx, 363, false)
			if err != nil {
				t.Fatal(err)
			}
			// A second issue remains usable and its creation preserves the tombstone.
			if _, err = f.store.CompareAndSwap(ctx, 364, "", []byte(`{"lease":{"issue_number":364}}`)); err != nil {
				t.Fatal(err)
			}
			after, err := f.store.ArchiveTombstone(ctx, 363, false)
			if err != nil || !reflect.DeepEqual(tomb, after) {
				t.Fatal("unrelated initial write lost tombstone")
			}
		})
	}
}

func TestArchivedStaleInitialCreationFence(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			// Capture exactly the admission generation used by initial CAS before the
			// archive transaction removes the live issue and changes the index.
			fence, err := f.store.initialArchiveFence(ctx, 363, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			before := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)")
			args := append([]string{"push", "origin", "--force-with-lease=" + layout.StateRef(363) + ":", m.StateTip + ":" + layout.StateRef(363)}, fence...)
			if err = f.store.push(ctx, args...); err == nil {
				t.Fatal("stale initial creation revived archived issue")
			}
			if got := f.git(f.remote, "for-each-ref", "--format=%(refname) %(objectname)"); got != before {
				t.Fatal("stale creation changed authority")
			}
		})
	}
}

func TestArchiveIndexMigrationRestore(t *testing.T) {
	f, m := archiveFixture(t, BranchLayout)
	ctx := context.Background()
	if err := f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := f.git(f.remote, "rev-parse", BranchLayout.ArchiveRef())
	report, err := f.store.MigrateLedgerRefs(ctx, time.Now(), false)
	if err != nil || len(report.Moves) != 1 {
		t.Fatalf("migration: %+v %v", report, err)
	}
	if got := f.git(f.remote, "rev-parse", HiddenLayout.ArchiveRef()); got != before {
		t.Fatal("migration rewrote archive index")
	}
	hidden := NewGitStoreWithLayout(f.clone, "origin", HiddenLayout)
	if _, err = hidden.RestoreArchive(ctx, 363, "octo/swarm", true); err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Refs {
		ref, _ := BranchLayout.counterpartRef(r.Source)
		if got := f.git(f.remote, "rev-parse", ref); got != r.SHA {
			t.Fatal("migration restore lost object")
		}
		if f.git(f.remote, "for-each-ref", "--format=%(objectname)", r.Source) != "" {
			t.Fatal("restore resurrected old namespace")
		}
	}
}

func TestArchiveDestinationSafety(t *testing.T) {
	f, m := archiveFixture(t, BranchLayout)
	ctx := context.Background()
	cases := []string{"file://" + f.remote, "https://secret-sentinel@example.invalid/archive.git"}
	link := filepath.Join(f.root, "archive-link.git")
	if err := os.Symlink(strings.TrimPrefix(m.Destination, "file://"), link); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, "file://"+link)
	for _, dest := range cases {
		err := f.store.CheckArchiveDestination(ctx, dest)
		if err == nil {
			t.Fatal("unsafe destination accepted")
		}
		if strings.Contains(err.Error(), "secret-sentinel") {
			t.Fatal("credential appeared in error")
		}
	}
	f.git(f.clone, "config", "url."+"file://"+f.remote+".insteadOf", "ssh://alias.invalid/source")
	if err := f.store.CheckArchiveDestination(ctx, "ssh://alias.invalid/source"); err == nil {
		t.Fatal("source URL alias accepted")
	}
}

func TestArchiveLostPushResponse(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			ctx := context.Background()
			real, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			script := `#!/bin/sh
if [ "$1" = push ] && [ "$3" = "$ARCHIVE_LOST_SOURCE" ]; then
 "$ARCHIVE_LOST_REAL" "$@" || exit $?
 echo 'lost push response' >&2
 exit 1
fi
exec "$ARCHIVE_LOST_REAL" "$@"
`
			if err = os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARCHIVE_LOST_REAL", real)
			t.Setenv("ARCHIVE_LOST_SOURCE", "file://"+f.remote)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err = f.store.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
				t.Fatalf("committed archive not recognized: %v", err)
			}
			if _, err = f.store.RestoreArchive(ctx, 363, "octo/swarm", true); err != nil {
				t.Fatalf("committed restore not recognized: %v", err)
			}
			for _, r := range m.Refs {
				if got := f.git(f.remote, "rev-parse", r.Source); got != r.SHA {
					t.Fatal("lost response changed restored objects")
				}
			}
		})
	}
}

func TestArchiveCanonicalCandidates(t *testing.T) {
	for _, layout := range []RefLayout{BranchLayout, HiddenLayout} {
		t.Run(layout.Name(), func(t *testing.T) {
			f, m := archiveFixture(t, layout)
			for _, ref := range []string{layout.StateRef(2), layout.StatePrefix() + "issue-01", layout.StatePrefix() + "issue-0", layout.StatePrefix() + "issue-999999999999999999999999", layout.StatePrefix() + "issue-4/nested", layout.StatePrefix() + "issue-4-suffix"} {
				f.git(f.remote, "update-ref", ref, m.StateTip)
			}
			before := f.git(f.clone, "for-each-ref")
			candidates, e := f.store.ArchiveCandidates(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if len(candidates) != 2 || candidates[0].Issue != 2 || candidates[1].Issue != 363 {
				t.Fatalf("%+v", candidates)
			}
			if before != f.git(f.clone, "for-each-ref") {
				t.Fatal("enumeration changed refs")
			}
			if _, e = f.store.ArchiveCommitTime(context.Background(), strings.Repeat("a", 40)); e == nil {
				t.Fatal("missing object accepted")
			}
		})
	}
}
func TestArchiveRetainedCutoffRejectsInconsistentAge(t *testing.T) {
	f, m := archiveFixture(t, BranchLayout)
	ctx := context.Background()
	stamp, e := f.store.ArchiveCommitTime(ctx, m.StateTip)
	if e != nil {
		t.Fatal(e)
	}
	m.Cutoff = &stamp
	// A structurally valid but age-inconsistent prior manifest must never authorize a retry.
	stop := errors.New("stop after copy")
	if e = f.store.Archive(ctx, m, func(context.Context) error { return stop }); !errors.Is(e, stop) {
		t.Fatalf("%v", e)
	}
	later := stamp.Add(24 * time.Hour)
	m.Cutoff = &later
	if e = f.store.Archive(ctx, m, func(context.Context) error { return nil }); e == nil {
		t.Fatal("age-inconsistent retained cutoff accepted")
	}
	if got := f.git(f.remote, "rev-parse", BranchLayout.StateRef(363)); got != m.StateTip {
		t.Fatal("source removed")
	}
}

func TestArchiveCutoffNormalizationKeepsIdentityExact(t *testing.T) {
	for _, field := range []string{"repository", "namespace", "destination", "outcome", "checkpoint", "state"} {
		t.Run(field, func(t *testing.T) {
			f, m := archiveFixture(t, BranchLayout)
			ctx := context.Background()
			stamp, e := f.store.ArchiveCommitTime(ctx, m.StateTip)
			if e != nil {
				t.Fatal(e)
			}
			c1 := stamp.Add(24 * time.Hour)
			m.Cutoff = &c1
			if e = f.store.Archive(ctx, m, func(context.Context) error { return errors.New("copied") }); e == nil {
				t.Fatal("expected post-copy failure")
			}
			c2 := c1.Add(24 * time.Hour)
			m.Cutoff = &c2
			switch field {
			case "repository":
				m.Repository = "other/repository"
			case "namespace":
				m.Namespace = "hidden"
			case "destination":
				m.Destination += "/../archive.git"
			case "outcome":
				m.PROutcome = "merged"
			case "checkpoint":
				m.Refs = m.Refs[1:]
			case "state":
				m.StateTip = strings.Repeat("a", 40)
			}
			before := f.git(f.remote, "for-each-ref")
			if e = f.store.Archive(ctx, m, func(context.Context) error { return nil }); e == nil {
				t.Fatal("identity mismatch accepted")
			}
			if before != f.git(f.remote, "for-each-ref") {
				t.Fatal("failed identity check changed source")
			}
		})
	}
}

func TestGitLoadArchiveAdmission(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		f, m := archiveFixture(t, layout)
		s := f.store
		ctx := context.Background()
		if _, _, err := s.Load(ctx, 363); err != nil {
			t.Fatal(err)
		}
		if err := s.Archive(ctx, m, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		var archived *ArchivedError
		if _, _, err := s.Load(ctx, 363); !errors.As(err, &archived) || archived.Issue != 363 || err.Error() != "issue #363 is archived; run swarm repo archive restore 363" {
			t.Fatalf("%v", err)
		}
		// A writer's mutable copy must never alias admission's cached map.
		idx, _, err := s.archiveIndex(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		delete(idx.Entries, 363)
		if _, _, err := s.Load(ctx, 363); !errors.As(err, &archived) {
			t.Fatalf("mutable index escaped: %v", err)
		}
		// Unrelated tombstones permit a missing issue to retain ErrNotFound.
		if _, _, err := s.Load(ctx, 364); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.RestoreArchive(ctx, 363, "octo/swarm", true); err != nil {
			t.Fatal(err)
		}
		if _, rev, err := s.Load(ctx, 363); err != nil || rev != m.StateTip {
			t.Fatalf("%s %v", rev, err)
		}
	})
}

func TestGitLoadArchiveCache(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		f := newLedgerFixture(t, layout)
		s := f.store(f.clone)
		ctx := context.Background()
		if _, err := s.CompareAndSwap(ctx, 1, "", []byte("state")); err != nil {
			t.Fatal(err)
		}
		// Concurrent cold-cache publication and subsequent reuse share immutable maps.
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := s.Load(ctx, 1); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		tip := f.git(f.remote, "rev-parse", layout.ArchiveRef())
		if s.admissionTip != tip || s.admissionIndex == nil {
			t.Fatal("cache not populated")
		}
		next, err := s.indexCommit(ctx, freshArchiveIndex(), tip)
		if err != nil {
			t.Fatal(err)
		}
		f.git(f.clone, "push", "origin", next+":"+layout.ArchiveRef())
		if _, _, err := s.Load(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if s.admissionTip != next {
			t.Fatal("cache did not replace old tip")
		}
		f.git(f.remote, "update-ref", "-d", layout.ArchiveRef())
		if _, _, err := s.Load(ctx, 2); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}
