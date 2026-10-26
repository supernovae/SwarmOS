package coordination

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckpointLineage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		b, e := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
		return strings.TrimSpace(string(b))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", remote)
	git("init")
	git("remote", "add", "origin", remote)
	s := NewGitStore(root)
	first, err := s.CompareAndSwap(ctx, 7, "", []byte(`{"lease":{"issue_number":7},"head_sha":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CompareAndSwap(ctx, 7, first, []byte(`{"lease":{"issue_number":7},"head_sha":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	heads, err := s.CheckpointLineage(ctx, 7, first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(heads, ","), "new") {
		t.Fatal("read beyond pinned revision")
	}
	heads, err = s.CheckpointLineage(ctx, 7, second)
	if err != nil || !strings.Contains(strings.Join(heads, ","), "old") {
		t.Fatalf("missing legacy history: %v %v", heads, err)
	}
	if _, err = s.CheckpointLineage(ctx, 8, second); err == nil {
		t.Fatal("wrong issue accepted")
	}
	if _, err = s.CheckpointLineage(ctx, 7, strings.Repeat("0", 40)); err == nil {
		t.Fatal("missing history accepted")
	}
	bad, err := s.CompareAndSwap(ctx, 7, second, []byte(`{`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckpointLineage(ctx, 7, bad); err == nil {
		t.Fatal("malformed history accepted")
	}
}
