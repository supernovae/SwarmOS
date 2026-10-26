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

func TestCheckpointRebases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if b, e := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); e != nil {
			t.Fatalf("git: %s %v", b, e)
		}
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("init", "--bare", remote)
	git("init")
	git("remote", "add", "origin", remote)
	s := NewGitStore(root)
	first, err := s.CompareAndSwap(ctx, 7, "", []byte(`{"lease":{"issue_number":7},"head_sha":"a","last_rebase":{"old_head":"a","new_head":"b","status":"completed","patch_identity":"unchanged","verified":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CompareAndSwap(ctx, 7, first, []byte(`{"lease":{"issue_number":7},"head_sha":"c","last_rebase":{"old_head":"b","new_head":"c","status":"completed","patch_identity":"changed","verified":true},"rebase_transitions":[{"old_head":"a","new_head":"b","status":"completed","patch_identity":"unchanged","verified":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.CheckpointHistory(ctx, 7, first)
	if err != nil || len(history.Rebases) != 1 || history.Rebases[0].NewHead != "b" {
		t.Fatalf("pinned history: %+v %v", history, err)
	}
	history, err = s.CheckpointHistory(ctx, 7, second)
	if err != nil || len(history.Rebases) != 2 {
		t.Fatalf("transitions not deduplicated: %+v %v", history, err)
	}
	if r := history.Rebases[0]; r.OldHead != "b" || r.PatchIdentity != "changed" {
		t.Fatalf("evidence mutated: %+v", r)
	}
	if strings.Join(history.Heads, ",") != "c,,a," {
		t.Fatalf("heads: %v", history.Heads)
	}
	if _, err = s.CheckpointHistory(ctx, 8, second); err == nil {
		t.Fatal("wrong issue accepted")
	}
	if _, err = s.CheckpointHistory(ctx, 7, strings.Repeat("0", 40)); err == nil {
		t.Fatal("missing history accepted")
	}
	bad, err := s.CompareAndSwap(ctx, 7, second, []byte(`{"lease":{"issue_number":7},"rebase_transitions":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckpointHistory(ctx, 7, bad); err == nil {
		t.Fatal("malformed history accepted")
	}
}
