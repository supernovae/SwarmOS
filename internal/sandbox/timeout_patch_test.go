package sandbox

import (
	"context"
	"errors"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeTimeoutPreservesPatchWithoutImport(t *testing.T) {
	dir := t.TempDir()
	writeWorkerFixture(t, dir, "changed", "dirty input\n")
	writeWorkerFixture(t, dir, "deleted", "gone\n")
	writeWorkerFixture(t, dir, "untracked", "original untracked\n")
	err := withNativeWorkspace(context.Background(), dir, WorkerOptions{Role: "developer", Limits: contract.AgentLimits{Wall: 100 * time.Millisecond}}, func(ctx context.Context, work, _ string) error {
		writeWorkerFixture(t, work, "changed", "finished part\n")
		writeWorkerFixture(t, work, "added", "new text without newline")
		if err := os.Remove(filepath.Join(work, "deleted")); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
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
	for _, attack := range []string{"source-file", "source-directory", "destination-file", "destination-directory"} {
		t.Run(attack, func(t *testing.T) {
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
