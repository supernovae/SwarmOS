package lease

import (
	"context"
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
