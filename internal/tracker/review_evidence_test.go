package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReviewEvidenceEndpointsAndBounds(t *testing.T) {
	calls := 0
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.URL.Path, "/pulls/17/") || r.URL.Query().Get("per_page") != "100" {
			t.Error(r.URL)
		}
		fmt.Fprint(w, `[{"id":3,"body":"finding","user":{"login":"reviewer"},"commit_id":"older","path":"file.go","line":7}]`)
	}))
	out, err := tr.GetReviewEvidence(t.Context(), 17)
	if err != nil || calls != 2 || len(out.Records) != 2 || out.Records[0].Kind != "reviews" || out.Records[1].Kind != "comments" || out.Records[1].Line != 7 {
		t.Fatalf("%+v %v calls=%d", out, err, calls)
	}
	if _, err = tr.GetReviewEvidence(t.Context(), 0); err == nil {
		t.Fatal("invalid number accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = tr.GetReviewEvidence(ctx, 17); err == nil {
		t.Fatal("cancellation ignored")
	}
}
func TestReviewEvidenceHTTPFailures(t *testing.T) {
	for _, kind := range []string{"auth", "bytes", "records", "body"} {
		t.Run(kind, func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "auth":
					w.WriteHeader(403)
				case "bytes":
					fmt.Fprint(w, strings.Repeat("x", (1<<20)+1))
				case "records":
					writeJSON(t, w, make([]ReviewEvidenceRecord, 101))
				case "body":
					fmt.Fprintf(w, `[{"body":%q}]`, strings.Repeat("界", 10000))
				}
			}))
			out, err := tr.GetReviewEvidence(t.Context(), 1)
			if kind == "body" {
				if err != nil || !out.Truncated || len(out.Records[0].Body) > ReviewBodyLimit {
					t.Fatalf("%+v %v", out, err)
				}
			} else if err == nil {
				t.Fatal("limit/failure ignored")
			}
		})
	}
}
func TestReviewEvidencePageBudget(t *testing.T) {
	calls := 0
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(t, w, make([]ReviewEvidenceRecord, 100))
	}))
	out, err := tr.GetReviewEvidence(t.Context(), 1)
	if err != nil || !out.Truncated || calls > 20 || len(out.Records) > 2000 {
		t.Fatalf("calls=%d records=%d err=%v", calls, len(out.Records), err)
	}
}
