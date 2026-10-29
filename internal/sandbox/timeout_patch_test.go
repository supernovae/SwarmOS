package sandbox

import (
	"context"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeTimeoutPreservesPatchWithoutImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeWorkerFixture(t, dir, "changed", "dirty input\n")
	writeWorkerFixture(t, dir, "deleted", "gone\n")
	writeWorkerFixture(t, dir, "untracked", "original untracked\n")
	clock := &fakeBudgetClock{now: time.Now()}
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	completion := make(chan error, 1)
	done := false
	t.Cleanup(func() {
		cancel()
		if done {
			return
		}
		select {
		case <-completion:
		case <-time.After(60 * time.Second):
			t.Error("native workspace did not finish after cancellation")
		}
	})
	go func() {
		completion <- withNativeWorkspaceClock(ctx, dir, WorkerOptions{Role: "developer", Limits: contract.AgentLimits{Wall: 100 * time.Millisecond}}, clock.clock(), func(ctx context.Context, work, _ string) error {
			if err := os.WriteFile(filepath.Join(work, "changed"), []byte("finished part\n"), 0600); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(work, "added"), []byte("new text without newline"), 0600); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
				return err
			}
			close(ready)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-ready:
	case err := <-completion:
		done = true
		t.Fatalf("native workspace completed before patch readiness: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("native workspace did not reach patch readiness")
	}
	select {
	case err := <-completion:
		done = true
		t.Fatalf("native workspace completed before clock advancement: %v", err)
	default:
	}
	clock.advance(100 * time.Millisecond)
	var err error
	select {
	case err = <-completion:
		done = true
	case <-time.After(30 * time.Second):
		t.Fatal("native workspace did not finish after budget expiration")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "100ms") || !strings.Contains(err.Error(), "developer-timeout.patch") {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(filepath.Join(dir, ".swarm", "active-output", "developer-timeout.patch"))
	if readErr != nil || !strings.Contains(string(data), "-dirty input") {
		t.Fatalf("%s %v", data, readErr)
	}
	original, _ := os.ReadFile(filepath.Join(dir, "changed"))
	if string(original) != "dirty input\n" {
		t.Fatal("imported changes")
	}
	// Check actual unified-diff applicability only in a separate fixture.
	fixture := t.TempDir()
	writeWorkerFixture(t, fixture, "changed", "dirty input\n")
	writeWorkerFixture(t, fixture, "deleted", "gone\n")
	writeWorkerFixture(t, fixture, "untracked", "original untracked\n")
	patch := filepath.Join(dir, ".swarm", "active-output", "developer-timeout.patch")
	cmd := gitfixture.Command(t, "", "apply", "--unsafe-paths", patch)
	cmd.Dir = fixture
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply: %s %v\n%s", output, err, data)
	}
	result, _ := os.ReadFile(filepath.Join(fixture, "added"))
	if string(result) != "new text without newline" {
		t.Fatalf("%q", result)
	}
}
func TestTimeoutPatchRejectsSymlinks(t *testing.T) {
	t.Parallel()
	for _, attack := range []string{"source-file", "source-directory", "destination-file", "destination-directory"} {
		t.Run(attack, func(t *testing.T) {
			t.Parallel()
			source, work, dest := t.TempDir(), t.TempDir(), t.TempDir()
			writeWorkerFixture(t, source, "file", "old\n")
			archive := filepath.Join(t.TempDir(), "input.tar")
			if _, err := packWorker(context.Background(), source, archive, nil, false); err != nil {
				t.Fatal(err)
			}
			writeWorkerFixture(t, work, "file", "new\n")
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(sentinel, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			var link, target string
			switch attack {
			case "source-file":
				os.Remove(filepath.Join(work, "file"))
				link, target = filepath.Join(work, "file"), sentinel
			case "source-directory":
				link, target = filepath.Join(work, "link"), filepath.Dir(sentinel)
			case "destination-file":
				os.MkdirAll(filepath.Join(dest, ".swarm", "active-output"), 0700)
				link, target = filepath.Join(dest, ".swarm", "active-output", "developer-timeout.patch"), sentinel
			case "destination-directory":
				os.MkdirAll(filepath.Join(dest, ".swarm"), 0700)
				link, target = filepath.Join(dest, ".swarm", "active-output"), filepath.Dir(sentinel)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skip(err)
			}
			if _, err := preserveTimeoutPatch(context.Background(), archive, work, dest, "developer"); err == nil {
				t.Fatal("accepted symlink")
			}
			data, _ := os.ReadFile(sentinel)
			if string(data) != "safe" {
				t.Fatal("changed outside sentinel")
			}
		})
	}
}
func TestTimeoutPatchBoundsAndBinaryOmissions(t *testing.T) {
	t.Parallel()
	source, work, dest := t.TempDir(), t.TempDir(), t.TempDir()
	archive := filepath.Join(t.TempDir(), "input.tar")
	if _, err := packWorker(context.Background(), source, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	writeWorkerFixture(t, work, "binary", "a\x00b")
	writeWorkerFixture(t, work, "huge", strings.Repeat("x", timeoutPatchBytes+1))
	writeWorkerFixture(t, work, "text", "recover this\n")
	result, err := preserveTimeoutPatch(context.Background(), archive, work, dest, "tester")
	if err != nil || !strings.Contains(result, "2 binary or oversized") {
		t.Fatalf("%s %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(dest, ".swarm", "active-output", "tester-timeout.patch"))
	if err != nil || len(data) > timeoutPatchBytes || !strings.Contains(string(data), "+recover this") {
		t.Fatalf("%s %v", data, err)
	}
}
func TestFailureAndValidationDoNotExport(t *testing.T) {
	t.Parallel()
	for _, discard := range []bool{false, true} {
		dir := t.TempDir()
		failure := errors.New("failed")
		err := withNativeWorkspace(context.Background(), dir, WorkerOptions{Role: "tester", DiscardChanges: discard}, func(ctx context.Context, work, _ string) error {
			if discard {
				if _, ok := ctx.Value(agentBudgetKey{}).(*agentBudget); ok {
					t.Fatal("validation budget")
				}
			}
			writeWorkerFixture(t, work, "changed", "no import")
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".swarm", "active-output")); !os.IsNotExist(err) {
			t.Fatalf("unexpected recovery: %v", err)
		}
	}
}

func TestTimeoutPatchContextEmptyFilesAndExecutable(t *testing.T) {
	t.Parallel()
	source, work, dest := t.TempDir(), t.TempDir(), t.TempDir()
	original := "first\nsecond\nthird\nfourth\nfifth\nsixth\nseventh\neighth\nninth\n"
	changed := strings.Replace(original, "fifth\n", "replacement\n", 1)
	writeWorkerFixture(t, source, "middle", original)
	writeWorkerFixture(t, source, "empty-deleted", "")
	archive := filepath.Join(t.TempDir(), "input.tar")
	if _, err := packWorker(context.Background(), source, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	writeWorkerFixture(t, work, "middle", changed)
	writeWorkerFixture(t, work, "empty-added", "")
	writeWorkerFixture(t, work, "script", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(work, "script"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := preserveTimeoutPatch(context.Background(), archive, work, dest, "developer"); err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	writeWorkerFixture(t, fixture, "middle", original)
	writeWorkerFixture(t, fixture, "empty-deleted", "")
	cmd := gitfixture.Command(t, "", "apply", "filepath-placeholder")
	cmd.Args[len(cmd.Args)-1] = filepath.Join(dest, ".swarm", "active-output", "developer-timeout.patch")
	cmd.Dir = fixture
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	actual, err := os.ReadFile(filepath.Join(fixture, "middle"))
	if err != nil || string(actual) != changed {
		t.Fatalf("%s %v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "empty-added")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "empty-deleted")); !os.IsNotExist(err) {
		t.Fatal("empty deletion lost")
	}
}

func TestTimeoutIdentityControlledReads(t *testing.T) {
	for _, attack := range []string{"stable", "growth", "shrink", "replace", "mode", "cancel", "error", "pre-cancel"} {
		t.Run(attack, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "asset", strings.Repeat("a", 65536))
			root, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if attack == "pre-cancel" {
				cancel()
			}
			calls, consumed := 0, 0
			got, err := timeoutRead(ctx, root, "asset", 32<<20, workerMaxBytes, func(f *os.File, b []byte) (int, error) {
				if len(b) > 32<<10 {
					t.Fatal("unbounded buffer")
				}
				n, err := f.Read(b)
				consumed += n
				calls++
				if calls == 1 {
					switch attack {
					case "growth":
						out, e := os.OpenFile(filepath.Join(dir, "asset"), os.O_APPEND|os.O_WRONLY, 0600)
						if e != nil {
							t.Fatal(e)
						}
						_, e = out.Write([]byte("x"))
						out.Close()
						if e != nil {
							t.Fatal(e)
						}
					case "shrink":
						if e := os.Truncate(filepath.Join(dir, "asset"), 1); e != nil {
							t.Fatal(e)
						}
					case "replace":
						writeWorkerFixture(t, dir, "replacement", strings.Repeat("a", 65536))
						if e := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "asset")); e != nil {
							t.Fatal(e)
						}
					case "mode":
						if e := os.Chmod(filepath.Join(dir, "asset"), 0755); e != nil {
							t.Fatal(e)
						}
					case "cancel":
						cancel()
					case "error":
						return n, errors.New("controlled read failure")
					}
				}
				return n, err
			})
			if consumed > 65537 {
				t.Fatalf("read %d bytes", consumed)
			}
			if attack == "stable" {
				if err != nil || !got.identity || !got.omitted || len(got.data) != 0 {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil || got.identity {
				t.Fatalf("accepted %s", attack)
			}
		})
	}
}

func TestTimeoutUnsupportedIdentity(t *testing.T) {
	dir := t.TempDir()
	writeWorkerFixture(t, dir, "asset", strings.Repeat("x", timeoutPatchBytes+1))
	root, err := safefs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	read := func(f *os.File, b []byte) (int, error) { return f.Read(b) }
	a, err := timeoutRead(t.Context(), root, "asset", 0, workerMaxBytes, read)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"equal", "bytes", "length", "mode", "unknown", "added", "deleted"} {
		t.Run(change, func(t *testing.T) {
			b := a
			before := map[string]timeoutFile{"asset": a, "text": {data: []byte("old\n")}}
			after := map[string]timeoutFile{"asset": b, "text": {data: []byte("new\n")}}
			switch change {
			case "bytes":
				b.digest[0] ^= 1
			case "length":
				b.size++
			case "mode":
				b.executable = !b.executable
			case "unknown":
				b.identity = false
			case "added":
				delete(before, "asset")
			case "deleted":
				delete(after, "asset")
			}
			if change != "deleted" {
				after["asset"] = b
			}
			patch, omitted, err := buildPatch(t.Context(), before, after)
			want := 1
			if change == "equal" {
				want = 0
			}
			if err != nil || omitted != want || !strings.Contains(string(patch), "+new") {
				t.Fatalf("omitted=%d err=%v", omitted, err)
			}
		})
	}
	// A retained copy and a retention-budget omission must compare by valid proof.
	writeWorkerFixture(t, dir, "small", "a\x00b")
	loaded, err := timeoutRead(t.Context(), root, "small", 0, workerMaxBytes, read)
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := timeoutRead(t.Context(), root, "small", 32<<20, workerMaxBytes, read)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTimeoutContent(loaded, omitted) {
		t.Fatal("mixed representation lost identity")
	}
	if sameTimeoutContent(timeoutFile{data: []byte{0}}, timeoutFile{data: []byte{0}}) {
		t.Fatal("unverified retained content proved identity")
	}
	if sameTimeoutContent(timeoutFile{omitted: true}, timeoutFile{omitted: true}) {
		t.Fatal("nil buffers proved identity")
	}
	if _, err := timeoutRead(t.Context(), root, "asset", 0, 1, read); err == nil {
		t.Fatal("aggregate budget ignored")
	}
}

func TestTimeoutSnapshotContentProof(t *testing.T) {
	for _, size := range []int{3, timeoutPatchBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			data := make([]byte, size)
			data[size-1] = 'a'
			writeWorkerFixture(t, dir, "asset", string(data))
			before, err := timeoutFiles(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, "asset"))
			if err != nil {
				t.Fatal(err)
			}
			// Identical size and timestamp are insufficient: the digest must differ.
			data[size-1] = 'b'
			if err := os.WriteFile(filepath.Join(dir, "asset"), data, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filepath.Join(dir, "asset"), info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, err := timeoutFiles(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			_, omitted, err := buildPatch(t.Context(), before, after)
			if err != nil || omitted != 1 {
				t.Fatalf("changed unsupported content accepted: %d %v", omitted, err)
			}
			after = before
			b := after["asset"]
			b.executable = !b.executable
			after = map[string]timeoutFile{"asset": b}
			_, omitted, err = buildPatch(t.Context(), before, after)
			if err != nil || omitted != 1 {
				t.Fatal("unsupported mode edit accepted")
			}
		})
	}
}

func TestTimeoutSnapshotRetentionPressure(t *testing.T) {
	dir := t.TempDir()
	for i := range 9 {
		writeWorkerFixture(t, dir, fmt.Sprintf("asset-%d", i), strings.Repeat("x", timeoutPatchBytes))
	}
	files, err := timeoutFiles(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var retained int
	omitted := 0
	for _, file := range files {
		retained += len(file.data)
		if file.omitted {
			omitted++
			if !file.identity {
				t.Fatal("missing omitted identity")
			}
		}
	}
	if retained != 32<<20 || omitted != 1 {
		t.Fatalf("retained=%d omitted=%d", retained, omitted)
	}
	patch, count, err := buildPatch(t.Context(), files, files)
	if err != nil || count != 0 || len(patch) != 0 {
		t.Fatalf("unchanged pressured snapshot: %d %v", count, err)
	}
}

func TestTimeoutSnapshotRejectsHardLinksAndFileLimit(t *testing.T) {
	for _, kind := range []string{"hardlink", "file-limit"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkerFixture(t, dir, "asset", "content")
			if kind == "hardlink" {
				if err := os.Link(filepath.Join(dir, "asset"), filepath.Join(dir, "alias")); err != nil {
					t.Skip(err)
				}
			} else {
				if err := os.Truncate(filepath.Join(dir, "asset"), workerFileBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := timeoutFiles(t.Context(), dir); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}
