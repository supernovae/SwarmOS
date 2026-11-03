package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// CanonicalArtifactBody excludes the coordinator's review-only guidance appendix.
func CanonicalArtifactBody(body string) string {
	if i := strings.Index(body, "\n<!-- swarm:guidance:start -->\n"); i >= 0 {
		return body[:i]
	}
	return body
}

const PlanFindingsStart = "<!-- swarm:plan-review-findings:start -->"
const PlanFindingsEnd = "<!-- swarm:plan-review-findings:end -->"

// PlanFinding is a cumulative, stable review record.
type PlanFinding struct {
	ID           string `json:"id"`
	Blocking     bool   `json:"blocking"`
	Disposition  string `json:"disposition"`
	Criterion    string `json:"criterion"`
	Trigger      string `json:"trigger"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
	Check        string `json:"check"`
	SupersededBy string `json:"superseded_by,omitempty"`
	ReversalNote string `json:"reversal_note,omitempty"`
}
type PlanFindings struct {
	Version  int           `json:"version"`
	Findings []PlanFinding `json:"findings"`
}

// uniqueJSON rejects duplicate object keys at every nesting level.
func uniqueJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			key := k.(string)
			if seen[key] {
				return fmt.Errorf("duplicate key %s", key)
			}
			seen[key] = true
			if e = uniqueJSON(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueJSON(d); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

func ParsePlanFindings(body string) (PlanFindings, error) {
	var r PlanFindings
	body = CanonicalArtifactBody(body)
	if strings.Count(body, PlanFindingsStart) != 1 || strings.Count(body, PlanFindingsEnd) != 1 {
		return r, fmt.Errorf("expected one plan findings block")
	}
	start := strings.Index(body, PlanFindingsStart) + len(PlanFindingsStart)
	end := strings.Index(body, PlanFindingsEnd)
	if end < start || end-start > 64<<10 {
		return r, fmt.Errorf("invalid findings block bounds")
	}
	raw := body[start:end]
	d := json.NewDecoder(strings.NewReader(raw))
	if err := uniqueJSON(d); err != nil {
		return r, err
	}
	if _, err := d.Token(); err != io.EOF {
		return r, fmt.Errorf("trailing findings JSON")
	}
	d = json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, err
	}
	if r.Version != 1 || r.Findings == nil {
		return r, fmt.Errorf("invalid findings version or list")
	}
	// Required members must be explicit, including blocking:false.
	var members struct {
		Findings []map[string]json.RawMessage `json:"findings"`
	}
	_ = json.Unmarshal([]byte(raw), &members)
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		return r, err
	}
	for key := range top {
		if key != "version" && key != "findings" {
			return r, fmt.Errorf("unknown findings field %s", key)
		}
	}
	allowed := map[string]bool{"id": true, "blocking": true, "disposition": true, "criterion": true, "trigger": true, "expected": true, "actual": true, "check": true, "superseded_by": true, "reversal_note": true}
	for _, fields := range members.Findings {
		for key, value := range fields {
			if !allowed[key] || string(value) == "null" {
				return r, fmt.Errorf("invalid finding field %s", key)
			}
		}
	}
	ids := map[string]PlanFinding{}
	idPattern := regexp.MustCompile(`^F[1-9][0-9]*$`)
	for i, f := range r.Findings {
		if !idPattern.MatchString(f.ID) {
			return r, fmt.Errorf("invalid finding ID")
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(f.ID, "F")); err != nil {
			return r, fmt.Errorf("finding ID too large")
		}
		if _, ok := ids[f.ID]; ok {
			return r, fmt.Errorf("duplicate finding ID")
		}
		ids[f.ID] = f
		if value, ok := members.Findings[i]["blocking"]; !ok || (string(value) != "true" && string(value) != "false") {
			return r, fmt.Errorf("missing blocking")
		}
		switch f.Disposition {
		case "open", "resolved", "withdrawn", "superseded":
		default:
			return r, fmt.Errorf("invalid disposition")
		}
		if f.Blocking {
			for _, v := range []string{f.Criterion, f.Trigger, f.Expected, f.Actual, f.Check} {
				if strings.TrimSpace(v) == "" {
					return r, fmt.Errorf("incomplete blocker %s", f.ID)
				}
			}
		}
		if f.Disposition != "superseded" && f.SupersededBy != "" {
			return r, fmt.Errorf("unexpected replacement")
		}
	}
	for _, f := range r.Findings {
		seen := map[string]bool{}
		for f.Disposition == "superseded" {
			if seen[f.ID] {
				return r, fmt.Errorf("supersede cycle")
			}
			seen[f.ID] = true
			next, ok := ids[f.SupersededBy]
			if !ok {
				return r, fmt.Errorf("missing supersede target")
			}
			f = next
		}
	}
	return r, nil
}

func ValidatePlanContinuity(current PlanFindings, previous []PlanFindings) error {
	latest := map[string]PlanFinding{}
	maxID := 0
	for _, r := range previous {
		for _, f := range r.Findings {
			latest[f.ID] = f
			n, _ := strconv.Atoi(strings.TrimPrefix(f.ID, "F"))
			if n > maxID {
				maxID = n
			}
		}
	}
	present := map[string]bool{}
	for _, f := range current.Findings {
		present[f.ID] = true
		old, ok := latest[f.ID]
		if !ok {
			n, e := strconv.Atoi(strings.TrimPrefix(f.ID, "F"))
			if e != nil || n <= maxID {
				return fmt.Errorf("recycled finding ID %s", f.ID)
			}
		} else {
			if old.Blocking != f.Blocking || old.Criterion != f.Criterion {
				return fmt.Errorf("finding identity changed: %s", f.ID)
			}
			if old.Disposition == "resolved" && f.Disposition == "open" && (strings.TrimSpace(f.ReversalNote) == "" || strings.TrimSpace(f.ReversalNote) == strings.TrimSpace(old.ReversalNote)) {
				return fmt.Errorf("reopened %s without new evidence", f.ID)
			}
		}
	}
	for id := range latest {
		if !present[id] {
			return fmt.Errorf("omitted prior finding %s", id)
		}
	}
	return nil
}

func (r PlanFindings) OpenBlockers() string {
	var b strings.Builder
	for _, f := range r.Findings {
		if f.Blocking && f.Disposition == "open" {
			fmt.Fprintf(&b, "\n%s\nCriterion: %s\nTrigger: %s\nExpected: %s\nActual: %s\nCheck: %s\n", f.ID, f.Criterion, f.Trigger, f.Expected, f.Actual, f.Check)
		}
	}
	if b.Len() > 0 {
		b.WriteString("\nRespond per ID in the next plan with the edit and verification, or a reasoned counterexample for reviewer reassessment.\n")
	}
	return b.String()
}

// PlannerReviewBody filters structured feedback while preserving legacy human prose.
func PlannerReviewBody(body string) string {
	if r, e := ParsePlanFindings(body); e == nil {
		return r.OpenBlockers()
	}
	return CanonicalArtifactBody(body)
}
