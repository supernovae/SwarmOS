package tracker

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Retrieval and storage bounds for code-scanning alerts. Renderers apply their
// own output budgets; these limits only bound what is kept in memory and JSON.
const (
	CodeScanningAlertLimit    = 50
	CodeScanningFieldLimit    = 1 << 10
	CodeScanningSeverityLimit = 64
	CodeScanningMessageLimit  = 4 << 10
	CodeScanningTextLimit     = 64 << 10
	codeScanningPages         = 10
	codeScanningPageSize      = 100
)

// CodeScanningUnavailable is fixed coordinator text; raw API errors are never shown.
const CodeScanningUnavailable = "unavailable (no access or not enabled)"

type CodeScanningAlert struct {
	Number   int64  `json:"number"`
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

// CodeScanningEvidence lists open alerts on a PR merge ref. Unavailable is
// distinct from an empty successful result. Omitted counts alerts beyond the
// retained ones; OmittedLowerBound means enumeration stopped at the page limit.
type CodeScanningEvidence struct {
	Ref               string              `json:"ref,omitempty"`
	Alerts            []CodeScanningAlert `json:"alerts"`
	Omitted           int                 `json:"omitted,omitempty"`
	OmittedLowerBound bool                `json:"omitted_lower_bound,omitempty"`
	TextTruncated     bool                `json:"text_truncated,omitempty"`
	Unavailable       bool                `json:"unavailable,omitempty"`
}

type CodeScanningEvidenceReader interface {
	GetCodeScanningAlerts(context.Context, int) (CodeScanningEvidence, error)
}

// CodeScanningMergeRef is the only ref queried: never a contributor-supplied ref.
func CodeScanningMergeRef(number int) string {
	return fmt.Sprintf("refs/pull/%d/merge", number)
}

func boundCodeScanningText(s string, limit int, truncated *bool) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	*truncated = true
	s = s[:limit]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// Normalize applies the retrieval bounds to evidence from any reader, so fake
// or provider data cannot bypass them. It is idempotent.
func (e CodeScanningEvidence) Normalize() CodeScanningEvidence {
	if e.Unavailable {
		return CodeScanningEvidence{Ref: e.Ref, Alerts: []CodeScanningAlert{}, Unavailable: true}
	}
	alerts := append([]CodeScanningAlert(nil), e.Alerts...)
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].Number < alerts[j].Number })
	out := e
	out.Alerts = []CodeScanningAlert{}
	if out.Omitted < 0 {
		out.Omitted = 0
	}
	budget := CodeScanningTextLimit
	for _, a := range alerts {
		if len(out.Alerts) == CodeScanningAlertLimit {
			out.Omitted++
			continue
		}
		a.RuleID = boundCodeScanningText(a.RuleID, CodeScanningFieldLimit, &out.TextTruncated)
		a.Severity = boundCodeScanningText(a.Severity, CodeScanningSeverityLimit, &out.TextTruncated)
		a.Path = boundCodeScanningText(a.Path, CodeScanningFieldLimit, &out.TextTruncated)
		a.Message = boundCodeScanningText(a.Message, CodeScanningMessageLimit, &out.TextTruncated)
		if a.Line < 0 {
			a.Line = 0
		}
		cost := len(a.RuleID) + len(a.Severity) + len(a.Path) + len(a.Message)
		if cost > budget {
			out.Omitted++
			continue
		}
		budget -= cost
		out.Alerts = append(out.Alerts, a)
	}
	return out
}

// GetCodeScanningAlerts reads open alerts for the PR merge ref from the
// tracker's own repository, which is the upstream in fork mode. Any failure
// returns an unavailable report and a sanitized error, never partial results.
func (g *GitHubTracker) GetCodeScanningAlerts(ctx context.Context, number int) (CodeScanningEvidence, error) {
	ref := CodeScanningMergeRef(number)
	unavailable := CodeScanningEvidence{Ref: ref, Alerts: []CodeScanningAlert{}, Unavailable: true}
	if number <= 0 {
		return unavailable, fmt.Errorf("code scanning alerts require a positive PR number")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out := CodeScanningEvidence{Ref: ref}
	for page := 1; page <= codeScanningPages; page++ {
		query := url.Values{"state": {"open"}, "ref": {ref}, "per_page": {fmt.Sprint(codeScanningPageSize)}, "page": {fmt.Sprint(page)}}
		var rows []struct {
			Number int64 `json:"number"`
			Rule   struct {
				ID                    string `json:"id"`
				Severity              string `json:"severity"`
				SecuritySeverityLevel string `json:"security_severity_level"`
			} `json:"rule"`
			Instance struct {
				Location struct {
					Path      string `json:"path"`
					StartLine int    `json:"start_line"`
				} `json:"location"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"most_recent_instance"`
		}
		if err := g.ciJSON(ctx, fmt.Sprintf("repos/%s/%s/code-scanning/alerts?%s", g.owner, g.repo, query.Encode()), &rows); err != nil {
			return unavailable, fmt.Errorf("code scanning alerts unavailable")
		}
		if len(rows) > codeScanningPageSize {
			return unavailable, fmt.Errorf("code scanning alerts page exceeds %d records", codeScanningPageSize)
		}
		for _, r := range rows {
			severity := r.Rule.SecuritySeverityLevel
			if severity == "" {
				severity = r.Rule.Severity
			}
			if severity == "" {
				severity = "unknown"
			}
			out.Alerts = append(out.Alerts, CodeScanningAlert{Number: r.Number, RuleID: r.Rule.ID, Severity: severity, Path: r.Instance.Location.Path, Line: r.Instance.Location.StartLine, Message: r.Instance.Message.Text})
		}
		// Keep memory bounded across pages: retain the lowest alert numbers.
		omitted := out.Omitted
		out.Omitted = 0
		out = out.Normalize()
		out.Omitted += omitted
		if len(rows) < codeScanningPageSize {
			break
		}
		if page == codeScanningPages {
			out.OmittedLowerBound = true
		}
	}
	return out, nil
}
