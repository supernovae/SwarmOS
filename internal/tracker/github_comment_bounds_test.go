package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCommentScanBoundsPagesAndBytes(t *testing.T) {
	t.Run("pages", func(t *testing.T) {
		calls := 0
		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Query().Get("since") != "2026-01-01T00:00:00Z" {
				t.Error("missing request timestamp filter")
			}
			rows := make([]ghCommentResponse, 100)
			writeJSON(t, w, rows)
		}))
		_, err := tr.ListCommentsSince(context.Background(), 1, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		if err == nil || !strings.Contains(err.Error(), "pages") || calls != maxCommentPages {
			t.Fatalf("page bound: calls=%d err=%v", calls, err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `[{"body":"`+strings.Repeat("x", maxCommentResponseBytes)+`"}]`)
		}))
		_, err := tr.ListComments(context.Background(), 1)
		if err == nil || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("missing byte bound: %v", err)
		}
	})
}
