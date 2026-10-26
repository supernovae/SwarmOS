package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func codeScanningRows(first, count int) []map[string]any {
	rows := make([]map[string]any, count)
	for i := range rows {
		rows[i] = map[string]any{
			"number":               first - i, // newest first, as GitHub lists them
			"rule":                 map[string]string{"id": "go/allocation-size-overflow", "severity": "warning", "security_severity_level": "high"},
			"most_recent_instance": map[string]any{"location": map[string]any{"path": "internal/contract/types.go", "start_line": 226}, "message": map[string]string{"text": "Size computation may overflow"}},
		}
	}
	return rows
}

func TestGitHubCodeScanningQueryOrderingAndCounts(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pages             int
		last              int
		wantAlerts        int
		wantOmitted       int
		wantLowerBound    bool
		wantRequestedPage int
	}{
		{"none", 1, 0, 0, 0, false, 1},
		{"one", 1, 1, 1, 0, false, 1},
		{"fifty", 1, 50, 50, 0, false, 1},
		{"fifty-one", 1, 51, 50, 1, false, 1},
		{"two pages", 2, 1, 50, 51, false, 2},
		{"page ceiling", 10, 100, 50, 950, true, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requested := 0
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if r.URL.Path != "/repos/octo/swarm/code-scanning/alerts" || q.Get("state") != "open" || q.Get("ref") != "refs/pull/124/merge" || q.Get("per_page") != "100" {
					t.Errorf("unexpected request %s", r.URL)
				}
				page, _ := strconv.Atoi(q.Get("page"))
				requested = page
				count := 100
				if page == tc.pages {
					count = tc.last
				}
				writeJSON(t, w, codeScanningRows(10000-100*(page-1), count))
			}))
			got, err := tr.GetCodeScanningAlerts(t.Context(), 124)
			if err != nil || got.Unavailable || len(got.Alerts) != tc.wantAlerts || got.Omitted != tc.wantOmitted || got.OmittedLowerBound != tc.wantLowerBound || requested != tc.wantRequestedPage || got.Ref != "refs/pull/124/merge" || got.Alerts == nil {
				t.Fatalf("%+v requested=%d err=%v", got, requested, err)
			}
			for i := 1; i < len(got.Alerts); i++ {
				if got.Alerts[i-1].Number >= got.Alerts[i].Number {
					t.Fatal("alerts not ordered by number")
				}
			}
			if len(got.Alerts) > 0 {
				a := got.Alerts[0]
				if a.RuleID != "go/allocation-size-overflow" || a.Severity != "high" || a.Path != "internal/contract/types.go" || a.Line != 226 || a.Message != "Size computation may overflow" {
					t.Fatalf("decoded %+v", a)
				}
			}
		})
	}
}

func TestGitHubCodeScanningSeverityFallbackAndMissingLocation(t *testing.T) {
	tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]any{
			{"number": 1, "rule": map[string]string{"id": "a", "severity": "error", "security_severity_level": "critical"}},
			{"number": 2, "rule": map[string]string{"id": "b", "severity": "note"}},
			{"number": 3, "rule": map[string]string{"id": "c"}},
		})
	}))
	got, err := tr.GetCodeScanningAlerts(t.Context(), 7)
	if err != nil || len(got.Alerts) != 3 {
		t.Fatal(got, err)
	}
	for i, want := range []string{"critical", "note", "unknown"} {
		if a := got.Alerts[i]; a.Severity != want || a.Path != "" || a.Line != 0 {
			t.Fatalf("%d: %+v", i, a)
		}
	}
}

func TestGitHubCodeScanningUnavailable(t *testing.T) {
	secret := "ghp_secret_token_value"
	for _, kind := range []string{"403", "404", "500", "malformed", "oversize", "records", "later-page", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			tr := newHTTPGitHubTracker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				switch kind {
				case "403", "404", "500":
					code, _ := strconv.Atoi(kind)
					w.WriteHeader(code)
					fmt.Fprintf(w, `{"message":"Resource not accessible %s \u001b[31m"}`, secret)
				case "malformed":
					fmt.Fprint(w, "{"+secret)
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", (2<<20)+1))
				case "records":
					writeJSON(t, w, codeScanningRows(500, 101))
				case "later-page":
					if page == "1" {
						writeJSON(t, w, codeScanningRows(500, 100))
						return
					}
					w.WriteHeader(500)
				default:
					writeJSON(t, w, codeScanningRows(500, 1))
				}
			}))
			ctx := t.Context()
			if kind == "cancelled" {
				// Stands in for the retrieval deadline without a wall-clock wait.
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := tr.GetCodeScanningAlerts(ctx, 1)
			if err == nil || !got.Unavailable || len(got.Alerts) != 0 || got.Alerts == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Resource") {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	if _, err := newHTTPGitHubTracker(t, http.NotFoundHandler()).GetCodeScanningAlerts(t.Context(), 0); err == nil {
		t.Fatal("non-positive PR number accepted")
	}
}

func TestCodeScanningNormalizeBounds(t *testing.T) {
	long := strings.Repeat("界", CodeScanningMessageLimit) // 3 bytes per rune
	in := CodeScanningEvidence{Alerts: []CodeScanningAlert{
		{Number: 2, RuleID: strings.Repeat("r", 2*CodeScanningFieldLimit), Severity: strings.Repeat("s", 100), Path: "bad\xff\xfepath", Line: -3, Message: long},
		{Number: 1, RuleID: "ok"},
	}}
	got := in.Normalize()
	if len(got.Alerts) != 2 || got.Alerts[0].Number != 1 || !got.TextTruncated {
		t.Fatalf("%+v", got)
	}
	a := got.Alerts[1]
	if len(a.RuleID) != CodeScanningFieldLimit || len(a.Severity) != CodeScanningSeverityLimit || len(a.Message) > CodeScanningMessageLimit || !utf8.ValidString(a.Message) || !utf8.ValidString(a.Path) || a.Line != 0 {
		t.Fatalf("unbounded %+v", a)
	}
	if again := got.Normalize(); len(again.Alerts) != 2 || again.Omitted != 0 || !again.TextTruncated {
		t.Fatal("normalize is not idempotent")
	}

	// Fifty maximal alerts exceed the aggregate text budget; drops are counted once.
	var full CodeScanningEvidence
	full.Omitted, full.OmittedLowerBound = 3, true
	for i := range 51 {
		full.Alerts = append(full.Alerts, CodeScanningAlert{Number: int64(i), RuleID: strings.Repeat("r", CodeScanningFieldLimit), Severity: strings.Repeat("s", CodeScanningSeverityLimit), Path: strings.Repeat("p", CodeScanningFieldLimit), Message: strings.Repeat("m", CodeScanningMessageLimit)})
	}
	got = full.Normalize()
	text := 0
	for _, a := range got.Alerts {
		text += len(a.RuleID) + len(a.Severity) + len(a.Path) + len(a.Message)
	}
	if text > CodeScanningTextLimit || len(got.Alerts)+got.Omitted != 51+3 || !got.OmittedLowerBound {
		t.Fatalf("aggregate %d retained %d omitted %d", text, len(got.Alerts), got.Omitted)
	}

	gone := CodeScanningEvidence{Alerts: []CodeScanningAlert{{Number: 1}}, Omitted: 4, Unavailable: true}.Normalize()
	if len(gone.Alerts) != 0 || gone.Omitted != 0 || !gone.Unavailable {
		t.Fatal("unavailable evidence kept partial results")
	}
}
