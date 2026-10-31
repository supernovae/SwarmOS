package tracker

import (
	"context"
	"errors"
	"fmt"
	"github.com/cli/go-gh/v2/pkg/api"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func inventoryTestBudget() InventoryBudget {
	return InventoryBudget{Records: 1000, Pages: 10, Bytes: 4 << 20, Reserve: func(int64) error { return nil }}
}

// Exercise the same encoded-string bound through every production entry point.
// The closing quote is included in quotedBytes, as in the transport reader.
func TestStorageInventoryStringLimitParity(t *testing.T) {
	for _, path := range []string{"pulls", "refs", "local"} {
		for _, size := range []int{60000, (512 << 10) - 1, 512 << 10} {
			t.Run(fmt.Sprintf("%s/%d", path, size), func(t *testing.T) {
				body := strings.Repeat("x", size)
				pr := fmt.Sprintf(`{"number":1,"state":"open","body":%q}`, body)
				visits := 0
				var run func(InventoryBudget) error
				if path == "local" {
					root := t.TempDir()
					file := filepath.Join(root, ".swarm", "local-tracker", "state.json")
					if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, []byte(`{"pull_requests":{"1":`+pr+`}}`), 0600); err != nil {
						t.Fatal(err)
					}
					tr := NewLocalTracker(root)
					run = func(b InventoryBudget) error {
						return tr.VisitPullRequests(t.Context(), b, func(*PullRequest) error { visits++; return nil })
					}
				} else {
					tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if path == "refs" {
							if strings.Contains(r.URL.Path, "/swarm/state/") {
								fmt.Fprint(w, `[]`)
								return
							}
							fmt.Fprintf(w, `[{"ref":"refs/heads/swarm-state/issue-1","object":{"sha":%q,"type":"commit"},"extra":%q}]`, strings.Repeat("a", 40), body)
						} else {
							fmt.Fprint(w, `[`+pr+`]`)
						}
					}))
					run = func(b InventoryBudget) error {
						if path == "refs" {
							return tr.VisitStateRefs(t.Context(), b, func(int, string) error { visits++; return nil })
						}
						return tr.VisitPullRequests(t.Context(), b, func(*PullRequest) error { visits++; return nil })
					}
				}
				b := inventoryTestBudget()
				var charge int64
				b.Workspace = func(n int64) error { charge = max(charge, n); return nil }
				err := run(b)
				if size == 512<<10 {
					if !errors.Is(err, ErrInventoryLimit) {
						t.Fatalf("accepted oversized string: %v", err)
					}
				} else if err != nil || visits != 1 {
					t.Fatalf("visits=%d: %v", visits, err)
				}
				if charge < int64(size)*4 {
					t.Fatalf("workspace not charged: %d", charge)
				}
				refused := errors.New("workspace refused")
				b.Workspace = func(int64) error { return refused }
				if err := run(b); !errors.Is(err, refused) {
					t.Fatalf("lost charging failure: %v", err)
				}
			})
		}
	}
}
func TestStoragePRVisitorPagesAndBounds(t *testing.T) {
	for _, limit := range []int{99, 100, 101, 102} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			calls, count := 0, 0
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Query().Get("state") != "all" {
					t.Error(r.Method, r.URL)
				}
				n, start := 100, 1
				if calls == 2 {
					n, start = 1, 101
				}
				var rows []map[string]any
				for i := 0; i < n; i++ {
					rows = append(rows, map[string]any{"number": start + i, "state": "closed", "merged_at": "2026-01-02T00:00:00Z", "head": map[string]string{"sha": "abc", "ref": "swarm/issue-1"}})
				}
				writeJSON(t, w, rows)
			}))
			b := inventoryTestBudget()
			b.Records = limit
			err := tr.VisitPullRequests(context.Background(), b, func(pr *PullRequest) error {
				count++
				if !pr.Merged || pr.HeadSHA != "abc" {
					t.Error(pr)
				}
				return nil
			})
			if limit < 101 && !errors.Is(err, ErrInventoryLimit) {
				t.Fatalf("%d %v", count, err)
			}
			if limit >= 101 && err != nil {
				t.Fatal(err)
			}
			if count != min(limit, 101) || calls > 2 {
				t.Fatal(count, calls)
			}
		})
	}
}
func TestStoragePRVisitorRejectsUnboundedJSON(t *testing.T) {
	for _, body := range []string{`[{"number":1,"state":"open","body":"` + strings.Repeat("x", (512<<10)+1) + `"}]`, strings.Repeat("[", 33), `[] trailing`} {
		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		if err := tr.VisitPullRequests(context.Background(), inventoryTestBudget(), func(*PullRequest) error { return nil }); err == nil {
			t.Fatal("accepted hostile JSON")
		}
	}
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) }))
	b := inventoryTestBudget()
	sentinel := errors.New("budget refused")
	b.Reserve = func(int64) error { return sentinel }
	if err := tr.VisitPullRequests(context.Background(), b, func(*PullRequest) error { return nil }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}
func TestStorageLocalVisitorDoesNotCreateOrRefresh(t *testing.T) {
	root := t.TempDir()
	tr := NewLocalTracker(root)
	if err := tr.VisitPullRequests(context.Background(), inventoryTestBudget(), func(*PullRequest) error { return nil }); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".swarm")); !os.IsNotExist(err) {
		t.Fatal("created storage", err)
	}
	path := filepath.Join(root, ".swarm", "local-tracker", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"issues":{"1":{"body":"ignored"}},"pull_requests":{"2":{"number":2,"state":"closed","head_sha":"persisted"}},"comments":{}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	count := 0
	err := tr.VisitPullRequests(context.Background(), inventoryTestBudget(), func(pr *PullRequest) error {
		count++
		if pr.Number != 2 || pr.HeadSHA != "persisted" {
			t.Error(pr)
		}
		return nil
	})
	if err != nil || count != 1 {
		t.Fatal(err, count)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("state changed")
	}
	names, _ := os.ReadDir(filepath.Dir(path))
	if len(names) != 1 {
		t.Fatal("created lock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = tr.VisitPullRequests(ctx, inventoryTestBudget(), func(*PullRequest) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type inventoryHostileBody struct {
	reads  int
	closed bool
}

func (b *inventoryHostileBody) Read(p []byte) (int, error) {
	b.reads++
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func (b *inventoryHostileBody) Close() error { b.closed = true; return nil }
func TestStoragePRVisitorNeverBuffersErrorBodies(t *testing.T) {
	body := &inventoryHostileBody{}
	calls := 0
	client, err := newInventoryRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "fixture", Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Status: "500 failed", Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: req}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tr := &GitHubTracker{owner: "local", repo: "fixture", inventoryClient: client}
	if err = tr.VisitPullRequests(context.Background(), inventoryTestBudget(), func(*PullRequest) error { return nil }); err == nil {
		t.Fatal("lost HTTP failure")
	}
	if calls != 1 || body.reads != 0 || !body.closed {
		t.Fatal(calls, body)
	}
	refused := inventoryTestBudget()
	refused.Reserve = func(int64) error { return ErrInventoryLimit }
	if err = tr.VisitPullRequests(context.Background(), refused, func(*PullRequest) error { return nil }); !errors.Is(err, ErrInventoryLimit) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("request started after budget refusal")
	}
}

func TestStorageStateRefsBoundedAndNamespaceChecked(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		calls, visits := 0, 0
		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			prefix := "refs/heads/swarm-state/"
			if strings.Contains(r.URL.Path, "/swarm/state/") {
				prefix = "refs/swarm/state/"
			}
			if calls == 2 && !conflict {
				fmt.Fprint(w, `[]`)
				return
			}
			writeJSON(t, w, []map[string]any{{"ref": prefix + "issue-9", "object": map[string]string{"sha": strings.Repeat("a", 40), "type": "commit"}}})
		}))
		err := tr.VisitStateRefs(context.Background(), inventoryTestBudget(), func(n int, sha string) error {
			visits++
			if n != 9 || sha != strings.Repeat("a", 40) {
				t.Fatal(n, sha)
			}
			return nil
		})
		if (err != nil) != conflict || visits != 1 || calls != 2 {
			t.Fatal(err, visits, calls)
		}
	}
}

func TestStoragePRVisitorFiveThousandRealisticResponses(t *testing.T) {
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		rows := []map[string]any{}
		for n := (page - 1) * 100; n < min(page*100, 5000); n++ {
			rows = append(rows, map[string]any{"number": n + 1, "state": "open", "body": strings.Repeat("b", 7000), "extra": strings.Repeat("x", 7000)})
		}
		writeJSON(t, w, rows)
	}))
	var charged, workspace int64
	charge := func(n int64) error {
		if n < 0 || charged+n > 64<<20 {
			return ErrInventoryLimit
		}
		charged += n
		return nil
	}
	count := 0
	err := tr.VisitPullRequests(t.Context(), InventoryBudget{Records: 5000, Pages: 100, Bytes: 128 << 20, Reserve: charge, Workspace: func(n int64) error {
		if n <= workspace {
			return nil
		}
		if err := charge(n - workspace); err != nil {
			return err
		}
		workspace = n
		return nil
	}}, func(*PullRequest) error { count++; return charge(4096) })
	if err != nil || count != 5000 {
		t.Fatal(count, charged, err)
	}
	t.Logf("5000 PRs charged=%d workspace=%d", charged, workspace)
}
