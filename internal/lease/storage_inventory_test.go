package lease

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/coordination"
)

func TestStorageInventoryStateProjection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	for _, tc := range []struct {
		body                   string
		valid, active, pending bool
	}{
		{`{"lease":{"version":1,"issue_number":1,"status":"active","expires_at":"2000-01-01T00:00:00Z"},"pr_revision":{},"pr_number":2}`, true, true, true},
		{`{"lease":{"version":1,"issue_number":1,"status":"complete"},"cleanup":null}`, true, false, false},
		{`{"lease":{"version":1,"issue_number":2}}`, false, false, false},
		{`{"lease":{"version":2,"issue_number":1}}`, false, false, false},
		{`{"lease":{"version":1,"issue_number":1},"other":` + strings.Repeat("[", 33) + `0` + strings.Repeat("]", 33) + `}`, false, false, false},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		state, err := ReadInventoryState(context.Background(), coordination.NewFileStore(root), 1, func(int64) error { return nil })
		if (err == nil) != tc.valid {
			t.Fatal(tc.body, err)
		}
		if tc.valid && (state.Active != tc.active || state.Pending != tc.pending) {
			t.Fatal(state)
		}
	}
}

func TestStorageInventoryStreamingProjection(t *testing.T) {
	base := `{"lease":{"version":1,"issue_number":1,"status":"completed","phase":"submitted"},"branch":"swarm/issue-1-test","pr_number":2,"submitted_head_sha":"abc","artifacts":[{"body":"`
	for _, size := range []int{150000, 4533441} {
		body := base + strings.Repeat("x", size) + `"}],"cleanup":null,"pr_revision":{}}`
		var charged int64
		data, err := ProjectInventory(context.Background(), strings.NewReader(body), func(n int64) error { charged += n; return nil })
		if err != nil || len(data) > 1024 || charged > 98304 {
			t.Fatalf("size=%d bytes=%d charge=%d err=%v", size, len(data), charged, err)
		}
		st, err := DecodeInventoryState(context.Background(), data, 1)
		if err != nil || !st.Completed || !st.Pending {
			t.Fatalf("%+v %v", st, err)
		}
		root := t.TempDir()
		dir := filepath.Join(root, ".swarm", "coordination")
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "issue-1.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		local, err := ReadInventoryState(context.Background(), coordination.NewFileStore(root), 1, func(int64) error { return nil })
		if err != nil || local != st {
			t.Fatalf("parity %+v %+v %v", local, st, err)
		}
	}
	for _, body := range []string{
		`{"lease":{"version":1,"issue_number":1},"branch":"a","branch":"b"}`,
		`{"lease":{"version":1,"issue_number":1},"Branch":"a"}`,
		`{"lease":{"version":1,"version":1,"issue_number":1}}`,
		`{"lease":{"version":1,"issue_number":1},"ignored":"bad\q"}`,
		`{"lease":{"version":1,"issue_number":1},"ignored":[1,]}`,
		`{"lease":{"version":1,"issue_number":1},"ignored":` + strings.Repeat("[", 32) + `0` + strings.Repeat("]", 32) + `}`,
		`{"lease":{"version":1,"issue_number":1},"ſubmitted_head_sha":"abc"}`,
		`{"lease":{"version":1,"issue_number":1}} true`,
		`{"lease":{"version":1,"issue_number":1},"ignored":"truncated`,
		`{"lease":{"version":1,"issue_number":1},"branch":"` + strings.Repeat("a", 4097) + `"}`,
	} {
		if _, err := DecodeInventoryState(context.Background(), []byte(body), 1); err == nil {
			t.Fatalf("accepted %s", body[:min(80, len(body))])
		}
	}
	sentinel := errors.New("budget refused")
	if _, err := ProjectInventory(context.Background(), strings.NewReader(`{}`), func(int64) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	// Input limit is independent of retained projection size, including direct reads.
	body := io.MultiReader(strings.NewReader(`{"ignored":"`), io.LimitReader(inventoryRepeatedByte{}, coordination.InventoryInputLimit), strings.NewReader(`"}`))
	if _, err := ProjectInventory(context.Background(), body, func(int64) error { return nil }); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatal(err)
	}
}

type inventoryRepeatedByte struct{}

func (inventoryRepeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestStorageInventoryStreamingExactCeilingAndLocalChange(t *testing.T) {
	prefix, suffix := `{"lease":{"version":1,"issue_number":1},"ignored":"`, `"}`
	for _, extra := range []int64{0, 1} {
		input := io.MultiReader(strings.NewReader(prefix), io.LimitReader(inventoryRepeatedByte{}, coordination.InventoryInputLimit-int64(len(prefix)+len(suffix))+extra), strings.NewReader(suffix))
		_, err := ProjectInventory(context.Background(), input, func(int64) error { return nil })
		if (err == nil) != (extra == 0) {
			t.Fatalf("extra=%d err=%v", extra, err)
		}
	}
	root := t.TempDir()
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	original := []byte(`{"lease":{"version":1,"issue_number":1}}`)
	store := coordination.NewFileStore(root)
	for _, replace := range []bool{false, true} {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		mutated := false
		_, err := ReadInventoryState(context.Background(), store, 1, func(int64) error {
			if mutated {
				return nil
			}
			mutated = true
			if replace {
				tmp := filepath.Join(dir, "replacement")
				if err := os.WriteFile(tmp, original, 0600); err != nil {
					return err
				}
				return os.Rename(tmp, path)
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(" ")
			closeErr := f.Close()
			return errors.Join(err, closeErr)
		})
		if err == nil {
			t.Fatal("changed descriptor/path accepted")
		}
	}
	if err := os.Truncate(path, coordination.InventoryInputLimit+1); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventoryState(context.Background(), store, 1, func(int64) error { return nil }); err == nil {
		t.Fatal("oversized local direct read accepted")
	}
}

// Nested evidence bodies are validated but never retained, even inside selected
// authority containers. Fields used by checkpoint lineage must survive.
func TestStorageInventoryNestedScalarProjection(t *testing.T) {
	body := `{"lease":{"version":1,"issue_number":1},"last_rebase":{"old_head":"old","new_head":"new","verified":true,"plans":[{"body":"` + strings.Repeat("p", 6474) + `"}],"artifacts":[{"body":"` + strings.Repeat("a", 11543) + `"}]},"rebase_transitions":[{"old_head":"old","new_head":"new","body":"` + strings.Repeat("b", 11543) + `"}]}`
	compact, err := ProjectInventory(context.Background(), strings.NewReader(body), func(int64) error { return nil })
	if err != nil || strings.Contains(string(compact), "body") || !strings.Contains(string(compact), `"old_head":"old"`) {
		t.Fatalf("%s %v", compact, err)
	}
	for _, invalid := range []string{
		strings.Replace(body, `"verified":true`, `"Verified":true`, 1),
		strings.Replace(body, `"old_head":"old"`, `"old_head":"`+strings.Repeat("x", 4097)+`"`, 1),
		strings.Replace(body, `"plans":[`, `"plans":[invalid,`, 1),
	} {
		if _, err := ProjectInventory(context.Background(), strings.NewReader(invalid), func(int64) error { return nil }); err == nil {
			t.Fatal("accepted invalid nested evidence")
		}
	}
}
