package tracker

import (
	"bytes"
	"os"
	"testing"
)

func TestNativeProjectionLocalStorageUnchanged(t *testing.T) {
	tr := NewLocalTracker(t.TempDir())
	for i := 0; i < 2; i++ {
		if refs, err := tr.ListBlockedBy(t.Context(), 1); err != nil || len(refs) != 0 {
			t.Fatalf("%v %v", refs, err)
		}
		if err := tr.LinkBlockedBy(t.Context(), 1, 2); err != nil {
			t.Fatal(err)
		}
		if err := tr.OrderSubIssues(t.Context(), 1, []int{3, 2}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(tr.statePath); !os.IsNotExist(err) {
		t.Fatalf("no-op created storage: %v", err)
	}
	parent, err := tr.CreateIssue(t.Context(), "parent", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := tr.CreateIssue(t.Context(), "child", "depends_on: [7]", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.LinkSubIssue(t.Context(), parent.Number, child.Number); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(tr.statePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_ = tr.LinkBlockedBy(t.Context(), child.Number, 7)
		_ = tr.OrderSubIssues(t.Context(), parent.Number, []int{child.Number})
		_, _ = tr.ListBlockedBy(t.Context(), child.Number)
	}
	after, err := os.ReadFile(tr.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("projection changed stored bytes")
	}
	reopened := NewLocalTracker(tr.repoRoot)
	got, err := reopened.GetIssue(t.Context(), child.Number)
	if err != nil || got.Body != child.Body {
		t.Fatalf("reopen: %v %v", got, err)
	}
}
