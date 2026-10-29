package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func dependencyRow(id int64, n int, repo string) map[string]any {
	return map[string]any{"id": id, "number": n, "html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, n)}
}

func TestNativeProjectionBlockedByIDPaginationAndRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		for _, localFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("race=%t/localFirst=%t", race, localFirst), func(t *testing.T) {
				linked := false
				posts := 0
				reads := 0
				tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/repos/octo/swarm/issues/7":
						writeJSON(t, w, map[string]any{"id": 7007, "number": 7})
					case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") && r.Method == "GET":
						reads++
						rows := []map[string]any{dependencyRow(9007, 7, "other/repo")}
						for i := 1; i < 100; i++ {
							rows = append(rows, dependencyRow(int64(10000+i), i+100, "other/repo"))
						}
						if linked {
							if localFirst {
								cross := rows[0]
								rows[0] = dependencyRow(7007, 7, "octo/swarm")
								rows = append(rows, cross)
							} else {
								rows = append(rows, dependencyRow(7007, 7, "octo/swarm"))
							}
						}
						if r.URL.Query().Get("page") == "1" {
							writeJSON(t, w, rows[:100])
						} else {
							writeJSON(t, w, rows[100:])
						}
					case strings.HasSuffix(r.URL.Path, "/dependencies/blocked_by") && r.Method == "POST":
						posts++
						var payload map[string]int64
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(payload, map[string]int64{"issue_id": 7007}) {
							t.Errorf("payload %v", payload)
						}
						linked = true
						if race {
							w.WriteHeader(422)
							writeJSON(t, w, map[string]string{"message": "validation failed"})
						} else {
							writeJSON(t, w, dependencyRow(7007, 7, "octo/swarm"))
						}
					default:
						t.Errorf("unexpected %s %s", r.Method, r.URL)
						http.NotFound(w, r)
					}
				}))
				for i := 0; i < 2; i++ {
					if err := tr.LinkBlockedBy(t.Context(), 11, 7); err != nil {
						t.Fatal(err)
					}
				}
				if posts != 1 || reads < 3 {
					t.Fatalf("posts=%d reads=%d", posts, reads)
				}
				refs, err := tr.ListBlockedBy(t.Context(), 11)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ref := range refs {
					if ref.String() == "octo/swarm#7" {
						found = true
					}
				}
				if !found {
					t.Fatal("missing qualified local reference")
				}
			})
		}
	}
}

func TestNativeProjectionErrors(t *testing.T) {
	for _, status := range []int{403, 404, 409, 422, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/issues/7") {
					writeJSON(t, w, map[string]any{"id": 7007})
				} else if r.Method == "GET" {
					writeJSON(t, w, []any{dependencyRow(9997, 7, "other/repo")})
				} else {
					w.WriteHeader(status)
					writeJSON(t, w, map[string]string{"message": "failed"})
				}
			}))
			if err := tr.LinkBlockedBy(t.Context(), 11, 7); err == nil {
				t.Fatal("unrelated failure swallowed")
			}
		})
	}
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, map[string]any{"number": 7}) }))
	for _, pair := range [][2]int{{0, 7}, {11, 0}, {7, 7}, {11, 7}} {
		if err := tr.LinkBlockedBy(t.Context(), pair[0], pair[1]); err == nil {
			t.Fatalf("accepted %v", pair)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := tr.LinkBlockedBy(ctx, 11, 7); err == nil {
		t.Fatal("ignored cancellation")
	}
	bad := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []any{map[string]any{"id": 77, "number": 7}})
	}))
	if _, err := bad.ListBlockedBy(t.Context(), 11); err == nil {
		t.Fatal("guessed repository identity")
	}
}

func TestNativeProjectionPriority(t *testing.T) {
	order := []int64{9009, 3003, 2002, 1001}
	patches := 0
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sub_issues"):
			var rows []map[string]any
			for _, id := range order {
				rows = append(rows, map[string]any{"id": id, "number": id / 1001})
			}
			writeJSON(t, w, rows)
		case strings.HasSuffix(r.URL.Path, "/priority"):
			if r.Method != "PATCH" {
				t.Fatal(r.Method)
			}
			patches++
			var p map[string]int64
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if len(p) != 2 || (p["before_id"] == 0) == (p["after_id"] == 0) {
				t.Fatalf("invalid anchor %v", p)
			}
			id := p["sub_issue_id"]
			var next []int64
			for _, v := range order {
				if v != id {
					next = append(next, v)
				}
			}
			var moved []int64
			for _, v := range next {
				if v == p["before_id"] {
					moved = append(moved, id)
				}
				moved = append(moved, v)
				if v == p["after_id"] {
					moved = append(moved, id)
				}
			}
			order = moved
			writeJSON(t, w, map[string]any{})
		default:
			n, err := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			if err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, map[string]any{"id": n * 1001, "number": n})
		}
	}))
	for i := 0; i < 2; i++ {
		if err := tr.OrderSubIssues(t.Context(), 10, []int{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(order, []int64{9009, 1001, 2002, 3003}) || patches != 6 {
		t.Fatalf("order %v patches %d", order, patches)
	}
	for _, children := range [][]int{nil, {1}} {
		if err := tr.OrderSubIssues(t.Context(), 10, children); err != nil {
			t.Fatal(err)
		}
	}
	if patches != 6 {
		t.Fatal("singleton mutation")
	}
	if err := tr.OrderSubIssues(t.Context(), 10, []int{1, 8}); err == nil {
		t.Fatal("missing membership accepted")
	}
}

func TestNativeProjectionPriorityFailures(t *testing.T) {
	for _, mode := range []string{"lookup", "patch", "collision", "missing-id"} {
		t.Run(mode, func(t *testing.T) {
			patches := 0
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/sub_issues") {
					if mode == "lookup" {
						w.WriteHeader(403)
						writeJSON(t, w, map[string]string{"message": "denied"})
						return
					}
					id := int64(1001)
					if mode == "collision" {
						id = 9991
					}
					if mode == "missing-id" {
						id = 0
					}
					writeJSON(t, w, []any{map[string]any{"id": id, "number": 1}, map[string]any{"id": 2002, "number": 2}})
				} else if strings.HasSuffix(r.URL.Path, "/priority") {
					patches++
					w.WriteHeader(500)
					writeJSON(t, w, map[string]string{"message": "failed"})
				} else {
					n, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
					writeJSON(t, w, map[string]any{"id": n * 1001, "number": n})
				}
			}))
			if err := tr.OrderSubIssues(t.Context(), 10, []int{1, 2}); err == nil {
				t.Fatal("failure hidden")
			}
			want := 0
			if mode == "patch" {
				want = 1
			}
			if patches != want {
				t.Fatalf("patches=%d want=%d", patches, want)
			}
		})
	}
}

func TestNativeProjectionListIdentityAndCancellation(t *testing.T) {
	for _, raw := range []string{"", "https://github.com/octo/swarm/issues/0", "https://github.com/octo/swarm/pull/7", "https://github.com/octo/swarm/issues/7?query=1"} {
		if _, err := IssueReference(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []any{map[string]any{"id": 77, "number": 7, "repository_url": "https://api.github.com/repos/other/repo"}})
	}))
	refs, err := tr.ListBlockedBy(t.Context(), 11)
	if err != nil || len(refs) != 1 || refs[0].String() != "other/repo#7" {
		t.Fatalf("%v %v", refs, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tr.ListBlockedBy(ctx, 11); err == nil {
		t.Fatal("ignored cancellation")
	}
	if err := tr.OrderSubIssues(ctx, 11, []int{1, 2}); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := tr.ListBlockedBy(t.Context(), 0); err == nil {
		t.Fatal("accepted invalid issue")
	}
}
