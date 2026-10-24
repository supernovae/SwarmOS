package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommittedPolicyTreeAndAncestry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runWorkspaceGit(t, root, "init", "-b", "main")
	runWorkspaceGit(t, root, "config", "user.name", "Test")
	runWorkspaceGit(t, root, "config", "user.email", "test@example.test")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		runWorkspaceGit(t, root, "add", ".")
		runWorkspaceGit(t, root, "commit", "-m", "fixture")
		return workspaceGitOutput(t, root, "rev-parse", "HEAD")
	}
	write(".swarm/config.yaml", "initial")
	base := commit()
	tree, err := PolicyTreeDigest(ctx, root, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := workspaceGitOutput(t, root, "rev-parse", base+":.swarm"); tree != want {
		t.Fatalf("tree=%s want=%s", tree, want)
	}
	write("source.go", "source")
	next := commit()
	got, err := PolicyTreeDigest(ctx, root, next)
	if err != nil || got != tree {
		t.Fatalf("source edit changed tree: %s %v", got, err)
	}
	write(".swarm/config.yaml", "uncommitted")
	got, err = PolicyTreeDigest(ctx, root, next)
	if err != nil || got != tree {
		t.Fatal("checkout influenced policy")
	}
	for _, name := range []string{"config.yaml", "validation/required-checks.yaml", "workflows/feature.yaml", "prompts/developer.md", "rules/rule.md", "contracts/schema.json", "newline\npath"} {
		old := next
		write(".swarm/"+name, "changed")
		next = commit()
		got, err = PolicyTreeDigest(ctx, root, next)
		if err != nil || got == tree {
			t.Fatalf("%s tree unchanged: %v", name, err)
		}
		paths, err := ChangedPolicyPaths(ctx, root, old, next)
		if err != nil || !reflect.DeepEqual(paths, []string{".swarm/" + name}) {
			t.Fatalf("paths=%q err=%v", paths, err)
		}
		tree = got
	}
	if ok, err := BaseIsAncestor(ctx, root, base, next); err != nil || !ok {
		t.Fatalf("ancestor=%t %v", ok, err)
	}
	if ok, err := BaseIsAncestor(ctx, root, next, base); err != nil || ok {
		t.Fatalf("rewind=%t %v", ok, err)
	}
	for _, bad := range []string{"HEAD", "--help", "0000000000000000000000000000000000000000"} {
		if _, err := PolicyTreeDigest(ctx, root, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	runWorkspaceGit(t, root, "rm", "-r", ".swarm")
	missing := commit()
	if _, err := PolicyTreeDigest(ctx, root, missing); err == nil {
		t.Fatal("accepted missing tree")
	}
	write(".swarm", "file")
	blob := commit()
	if _, err := PolicyTreeDigest(ctx, root, blob); err == nil {
		t.Fatal("accepted blob policy")
	}
	if _, err := BaseIsAncestor(ctx, root, base, "0000000000000000000000000000000000000000"); err == nil {
		t.Fatal("missing ancestry object accepted")
	}
}
