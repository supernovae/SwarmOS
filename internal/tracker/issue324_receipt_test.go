package tracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIssue324CreationReceipt(t *testing.T) {
	root := t.TempDir()
	tr := NewLocalTracker(root)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := tr.CreateIssue(context.Background(), "task", "body", nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var before []byte
	if err := tr.withLock(false, func(st *localState) error {
		if len(st.Issues) != 12 || len(st.ResumeReceipts) != 12 {
			t.Fatalf("allocation: %+v", st)
		}
		seen := map[string]bool{}
		for n, r := range st.ResumeReceipts {
			if r.Version != 1 || r.Issue != n || r.Status != "created" || r.Authority == "" || len(r.Lifecycle) != 64 || seen[r.Lifecycle] {
				t.Fatalf("receipt: %+v", r)
			}
			seen[r.Lifecycle] = true
		}
		before, _ = json.Marshal(st.ResumeReceipts)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CreateTypedIssue(context.Background(), "Epic", "body", nil, "Epic"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.GetIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := tr.withLock(false, func(st *localState) error {
		after, _ := json.Marshal(st.ResumeReceipts)
		if string(before) != string(after) {
			t.Fatal("read/typed allocation changed receipts")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A missing counter must never overwrite existing issues or retained receipts.
	if err := tr.withLock(true, func(st *localState) error { delete(st.Issues, 1); st.NextIssueNum = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CreateIssue(context.Background(), "collision", "body", nil); err == nil {
		t.Fatal("reused retained lifecycle")
	}
	b, err := os.ReadFile(filepath.Join(root, ".swarm/local-tracker/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "resume_receipts")
	raw["next_issue_num"] = json.RawMessage("14")
	b, _ = json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(root, ".swarm/local-tracker/state.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CreateIssue(context.Background(), "new", "body", nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.withLock(false, func(st *localState) error {
		if len(st.ResumeReceipts) != 1 || st.ResumeReceipts[14] == nil {
			t.Fatal("retroactively enrolled legacy issue")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIssue324MissingAllocationCounter(t *testing.T) {
	root := t.TempDir()
	tr := NewLocalTracker(root)
	if _, err := tr.CreateIssue(context.Background(), "first", "body", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".swarm/local-tracker/state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "next_issue_num")
	doc["issues"] = json.RawMessage(`{}`)
	doc["resume_receipts"] = json.RawMessage(`{}`)
	b, _ = json.Marshal(doc)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CreateIssue(context.Background(), "replacement", "body", nil); err == nil {
		t.Fatal("missing allocator counter treated as fresh storage")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(b) {
		t.Fatal("failed allocation changed authority")
	}
}

func TestIssue324ReceiptCommitUsesPinnedDirectory(t *testing.T) {
	root := t.TempDir()
	tr := NewLocalTracker(root)
	iss, err := tr.CreateIssue(context.Background(), "task", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".swarm", "local-tracker")
	old := dir + "-moved"
	if err := tr.WithResumeReceipt(iss.Number, func(r *ResumeReceipt, _ *Issue, commit func() error) error {
		if r == nil {
			t.Fatal("creation receipt absent")
		}
		if err := os.Rename(dir, old); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "sentinel"), []byte("replacement"), 0600); err != nil {
			return err
		}
		r.Status = "denied"
		return commit()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("write escaped locked directory: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(old, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state localState
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	if state.ResumeReceipts[iss.Number].Status != "denied" {
		t.Fatal("locked receipt was not updated")
	}
}
