package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryPreviewBoundedAndNonCreating(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(root)
	reserve := func(int64) error { return nil }
	if _, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve, testInventoryDecoder); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".swarm")); !os.IsNotExist(err) {
		t.Fatal("created storage")
	}
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	if err := os.WriteFile(path, []byte(`{"lease":{"issue_number":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	bytes, err := store.LoadInventoryPreview(context.Background(), 1, 1024, reserve, testInventoryDecoder)
	if err != nil || len(bytes) == 0 {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("changed state")
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 {
		t.Fatal("created lock")
	}
	sentinel := errors.New("reserve refused")
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1024, func(int64) error { return sentinel }, testInventoryDecoder); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err = store.LoadInventoryPreview(context.Background(), 1, 1, reserve, testInventoryDecoder); err == nil {
		t.Fatal("accepted oversized input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.LoadInventoryPreview(ctx, 1, 1024, reserve, testInventoryDecoder); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInventorySnapshotCoherenceAndNoLocks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".swarm", "coordination")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "issue-1.json")
	if err := os.WriteFile(path, []byte(`{"lease":{"version":1,"issue_number":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(root)
	states, err := store.ReadInventorySnapshot(context.Background(), 10, func(int64) error { return nil }, testInventoryDecoder)
	if err != nil || len(states) != 1 {
		t.Fatalf("%v %v", states, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("created lock", files, err)
	}
	mutated := false
	_, err = store.ReadInventorySnapshot(context.Background(), 10, func(int64) error {
		if !mutated {
			mutated = true
			tmp := filepath.Join(dir, "replacement")
			if e := os.WriteFile(tmp, []byte("replacement"), 0600); e != nil {
				return e
			}
			return os.Rename(tmp, path)
		}
		return nil
	}, testInventoryDecoder)
	if err == nil {
		t.Fatal("mixed snapshot accepted")
	}
	if _, err := store.ReadInventorySnapshot(context.Background(), 0, func(int64) error { return nil }, testInventoryDecoder); err == nil {
		t.Fatal("ignored entry bound")
	}
}

func testInventoryDecoder(ctx context.Context, r io.Reader, reserve func(int64) error) ([]byte, error) {
	if err := reserve(8192); err != nil {
		return nil, err
	}
	var v map[string]any
	err := json.NewDecoder(r).Decode(&v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func TestInventoryStreamingLimitsAndErrors(t *testing.T) {
	reserve := func(int64) error { return nil }
	// An exact input ceiling probes EOF; a single additional byte is refused.
	for _, extra := range []string{"", " "} {
		r := &inventoryReader{ctx: context.Background(), r: strings.NewReader(`{}` + extra), remaining: 2}
		_, err := io.ReadAll(r)
		if (err == nil) != (extra == "") {
			t.Fatal(extra, err)
		}
	}
	store := NewGitStore(t.TempDir())
	for _, sha := range []string{"HEAD", strings.Repeat("g", 40), "refs/heads/main"} {
		if _, err := store.LoadInventoryObject(context.Background(), sha, reserve, testInventoryDecoder); err == nil {
			t.Fatal(sha)
		}
	}
	realPath := os.Getenv("PATH")
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+realPath)
	script := filepath.Join(dir, "git")
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		script string
		valid  bool
	}{
		{"printf 'diagnostic' >&2\nprintf '{\"lease\":{\"issue_number\":1}}'\n", true},
		{"printf 'underlying Git failure' >&2\nexit 42\n", false},
		{"i=0; while [ $i -lt 10000 ]; do printf 'hostile diagnostic' >&2; i=$((i+1)); done\nexit 42\n", false},
	} {
		if err := os.WriteFile(script, []byte("#!/bin/sh\n"+tc.script), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := store.LoadInventoryObject(context.Background(), sha, reserve, testInventoryDecoder)
		if (err == nil) != tc.valid {
			t.Fatalf("data=%s err=%v", data, err)
		}
		if err != nil && (len(err.Error()) > 5000 || strings.Contains(err.Error(), "signal: killed")) {
			t.Fatal(err)
		}
		if strings.Contains(tc.script, "underlying Git failure") && (err == nil || !strings.Contains(err.Error(), "underlying Git failure")) {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /usr/bin/yes x\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("projection budget exhausted")
	refusing := func(context.Context, io.Reader, func(int64) error) ([]byte, error) { return nil, sentinel }
	if _, err := store.LoadInventoryObject(context.Background(), sha, reserve, refusing); !errors.Is(err, sentinel) || strings.Contains(err.Error(), "signal: killed") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := func(context.Context, io.Reader, func(int64) error) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	}
	if _, err := store.LoadInventoryObject(ctx, sha, reserve, cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Both calls returned only after Wait reaped the child; no helper survives.
}

func TestGitInventorySnapshotLayouts(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		f := newLedgerFixture(t, layout)
		ctx := context.Background()
		want := map[int]string{1: `{"issue":1}`, 23: `{"issue":23}`}
		for n, payload := range want {
			sha, err := f.store(f.clone).CompareAndSwap(ctx, n, "", []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			// The consumer predates these commits and has never fetched ledger objects.
			if err := gitfixture.Command(t, f.second, "cat-file", "-e", sha).Run(); err == nil {
				t.Fatal("consumer already has state object")
			}
		}
		got, err := f.store(f.second).ReadInventorySnapshot(ctx, true, 2, func(int64) error { return nil }, testInventoryDecoder)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("states: %v", got)
		}
		for n, payload := range want {
			if string(got[n]) != payload {
				t.Fatalf("issue %d: %s", n, got[n])
			}
		}
	})
}

// inventoryGitStub permits only the two combined advertisements, pinned object
// fetches and object reads. Unexpected Git commands fail the test.
func inventoryGitStub(t *testing.T, first, second string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	count := filepath.Join(dir, "listed")
	script := "#!/bin/sh\n"
	script += "printf '%s\\n' \"$*\" >> '" + log + "'\n"
	script += "case \"$1\" in\nls-remote)\n"
	script += "[ \"$*\" = 'ls-remote --refs origin refs/heads/swarm-state/* refs/swarm/state/*' ] || exit 90\n"
	script += "if [ -f '" + count + "' ]; then cat '" + filepath.Join(dir, "second") + "'; else touch '" + count + "'; cat '" + filepath.Join(dir, "first") + "'; fi;;\n"
	script += "fetch) [ \"$2 $3 $4 $5\" = '--no-tags --no-write-fetch-head origin " + strings.Repeat("a", 40) + "' ] || exit 91;;\n"
	script += "show) printf '{}';;\n*) exit 92;;\nesac\n"
	for name, data := range map[string]string{"git": script, "first": first, "second": second} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestGitInventorySnapshotRejectsRefs(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		sha := strings.Repeat("a", 40)
		row := sha + "\t" + layout.StateRef(1) + "\n"
		other := sha + "\t" + layout.Counterpart().StateRef(1) + "\n"
		cases := map[string]string{"counterpart": other, "mixed": row + other, "duplicate": row + row,
			"duplicate-other-sha": row + strings.Repeat("b", 40) + "\t" + layout.StateRef(1) + "\n",
			"bad-sha":             "xyz\t" + layout.StateRef(1) + "\n", "bad-row": sha + "\n"}
		for _, suffix := range []string{"abc", "0", "01", "1/extra"} {
			cases[suffix] = sha + "\t" + layout.StatePrefix() + "issue-" + suffix + "\n"
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) {
				inventoryGitStub(t, raw, raw)
				got, err := NewGitStoreWithLayout(t.TempDir(), "", layout).ReadInventorySnapshot(context.Background(), true, 10, func(int64) error { return nil }, testInventoryDecoder)
				if err == nil || got != nil {
					t.Fatalf("accepted %q: %v %v", raw, got, err)
				}
			})
		}
	})
}

func TestGitInventorySnapshotRefresh(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		row := strings.Repeat("a", 40) + "\t" + layout.StateRef(1) + "\n"
		cases := map[string]string{"unchanged": row, "removed": "", "added": row + strings.Repeat("b", 40) + "\t" + layout.StateRef(2) + "\n",
			"replaced": strings.Repeat("b", 40) + "\t" + layout.StateRef(1) + "\n"}
		for name, fresh := range cases {
			t.Run(name, func(t *testing.T) {
				log := inventoryGitStub(t, row, fresh)
				_, err := NewGitStoreWithLayout(t.TempDir(), "", layout).ReadInventorySnapshot(context.Background(), true, 10, func(int64) error { return nil }, testInventoryDecoder)
				if name == "unchanged" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "state snapshot changed during refresh") {
					t.Fatal(err)
				}
				calls, e := os.ReadFile(log)
				if e != nil {
					t.Fatal(e)
				}
				if strings.Count(string(calls), "ls-remote ") != 2 || strings.Count(string(calls), "fetch ") != 1 {
					t.Fatalf("calls: %s", calls)
				}
			})
		}
	})
}

func TestGitInventorySnapshotLimits(t *testing.T) {
	bothLayouts(t, func(t *testing.T, layout RefLayout) {
		row := strings.Repeat("a", 40) + "\t" + layout.StateRef(1) + "\n"
		for _, limit := range []int{0, 1} {
			t.Run(fmt.Sprint("records-", limit), func(t *testing.T) {
				inventoryGitStub(t, row, row)
				_, err := NewGitStoreWithLayout(t.TempDir(), "", layout).ReadInventorySnapshot(context.Background(), true, limit, func(int64) error { return nil }, testInventoryDecoder)
				if (err == nil) != (limit == 1) {
					t.Fatal(err)
				}
			})
		}
		// Reservation sequence: advertisement, entry, fetch, advertisement, entry,
		// object reader and decoder. Refusal at each stage must preserve identity.
		for fail := 1; fail <= 7; fail++ {
			t.Run(fmt.Sprint("reserve-", fail), func(t *testing.T) {
				inventoryGitStub(t, row, row)
				sentinel := errors.New("reservation refused")
				calls := 0
				_, err := NewGitStoreWithLayout(t.TempDir(), "", layout).ReadInventorySnapshot(context.Background(), true, 1, func(int64) error {
					calls++
					if calls == fail {
						return sentinel
					}
					return nil
				}, testInventoryDecoder)
				if !errors.Is(err, sentinel) {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			})
		}
		t.Run("advertisement-overflow", func(t *testing.T) {
			raw := strings.Repeat(row, 4000)
			inventoryGitStub(t, raw, raw)
			if _, err := NewGitStoreWithLayout(t.TempDir(), "", layout).ReadInventorySnapshot(context.Background(), true, 10000, func(int64) error { return nil }, testInventoryDecoder); err == nil {
				t.Fatal("accepted oversized advertisement")
			}
		})
		for _, extra := range []string{"", " "} {
			r := &inventoryReader{ctx: context.Background(), r: strings.NewReader("{}" + extra), remaining: 2}
			if _, err := io.ReadAll(r); (err == nil) != (extra == "") {
				t.Fatal(err)
			}
		}
	})
}
