package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Both native and harness-tool/OCI inputs use packWorker. Exercise the actual
// native snapshot constructor as well, with coordinator metadata in the input.
func TestCheckpointCompletionWorkerIsolation(t *testing.T) {
	source := cacheTestRepo(t)
	userCache := useUserCache(t)
	writeWorkerFixture(t, source, "source.txt", "source")
	for _, name := range []string{"key/material", "issue-467.json"} {
		writeWorkerFixture(t, source, ".git/swarm-checkpoint-completion/"+name, "coordinator-only")
	}
	archive := filepath.Join(t.TempDir(), "input.tar")
	manifest, err := packWorker(t.Context(), source, archive, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for name := range manifest {
		if strings.Contains(name, "swarm-checkpoint-completion") {
			t.Fatal("receipt exported")
		}
	}
	err = withNativeWorkspace(t.Context(), source, WorkerOptions{Role: "developer"}, func(_ context.Context, work, scratch string) error {
		if _, err := os.Lstat(filepath.Join(work, ".git")); !os.IsNotExist(err) {
			t.Fatal("Git metadata reached native worker")
		}
		caches := mustPrepare(t, AgentBuildCache, source, userCache, scratch)
		policy := strings.Join(codexPolicy("/trusted/codex", work, scratch, false, caches, Dependencies{}), " ")
		if strings.Contains(policy, strconv.Quote(source)) {
			t.Fatal("coordinator input root granted to agent")
		}
		writable := []string{work, scratch}
		for _, cache := range caches {
			writable = append(writable, cache.Dir)
		}
		for _, root := range writable {
			private := filepath.Join(source, ".git/swarm-checkpoint-completion")
			if isSubpath(private, root) || private == root {
				t.Fatal("receipt beneath an agent writable root")
			}
			if !strings.Contains(policy, strconv.Quote(root)+`="write"`) {
				t.Fatal("test missed a writable root")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key/material", "issue-467.json"} {
		data, err := os.ReadFile(filepath.Join(source, ".git/swarm-checkpoint-completion", name))
		if err != nil || string(data) != "coordinator-only" {
			t.Fatal("receipt changed during worker import")
		}
	}
}
