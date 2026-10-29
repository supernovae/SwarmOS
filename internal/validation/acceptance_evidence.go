package validation

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	AcceptanceEvidenceStart    = "<!-- swarm:acceptance-evidence:start -->"
	AcceptanceEvidenceEnd      = "<!-- swarm:acceptance-evidence:end -->"
	MaxAcceptanceEvidenceBytes = 64 * 1024
	AcceptanceEvidenceKey      = "acceptance_evidence"
)

// AcceptanceSnapshot is bounded advisory content, never a validation verdict.
type AcceptanceSnapshot struct {
	State string `json:"state"`
	Block string `json:"block,omitempty"`
}

type AcceptanceWarning struct {
	Index     int    `json:"index"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

type acceptanceEntry struct {
	Index     int    `json:"index"`
	Criterion string `json:"criterion"`
	Status    string `json:"status"`
	Evidence  string `json:"evidence"`
}

type acceptanceMapping struct {
	Version  int               `json:"version"`
	Criteria []json.RawMessage `json:"criteria"`
}

func decodeAcceptance(block string) (acceptanceMapping, bool) {
	var m acceptanceMapping
	if len(block) > MaxAcceptanceEvidenceBytes || json.Unmarshal([]byte(block), &m) != nil || m.Version != 1 || m.Criteria == nil {
		return m, false
	}
	return m, true
}

// CaptureAcceptanceEvidence reads only the separately delivered tester report.
func CaptureAcceptanceEvidence(body string) AcceptanceSnapshot {
	missing := AcceptanceSnapshot{State: "missing"}
	bad := AcceptanceSnapshot{State: "malformed"}
	if !strings.Contains(body, AcceptanceEvidenceStart) && !strings.Contains(body, AcceptanceEvidenceEnd) {
		return missing
	}
	if strings.Count(body, AcceptanceEvidenceStart) != 1 || strings.Count(body, AcceptanceEvidenceEnd) != 1 {
		return bad
	}
	start := strings.Index(body, AcceptanceEvidenceStart)
	end := strings.Index(body, AcceptanceEvidenceEnd)
	if end < start {
		return bad
	}
	// Delimiters must occupy standalone lines.
	for _, pos := range []struct{ start, end int }{{start, start + len(AcceptanceEvidenceStart)}, {end, end + len(AcceptanceEvidenceEnd)}} {
		left := strings.LastIndex(body[:pos.start], "\n") + 1
		right := strings.Index(body[pos.end:], "\n")
		if strings.TrimSpace(body[left:pos.start]) != "" {
			return bad
		}
		if right < 0 {
			right = len(body) - pos.end
		}
		if strings.TrimSpace(body[pos.end:pos.end+right]) != "" {
			return bad
		}
	}
	raw := body[start+len(AcceptanceEvidenceStart) : end]
	if len(raw) > MaxAcceptanceEvidenceBytes {
		return bad
	}
	if _, ok := decodeAcceptance(raw); !ok {
		return bad
	}
	return AcceptanceSnapshot{State: "valid", Block: raw}
}

func AcceptanceSnapshotFromMetadata(metadata map[string]string) AcceptanceSnapshot {
	var s AcceptanceSnapshot
	raw := metadata[AcceptanceEvidenceKey]
	if raw == "" {
		return AcceptanceSnapshot{State: "missing"}
	}
	// JSON escaping can expand the bounded block up to sixfold.
	if len(raw) > 6*MaxAcceptanceEvidenceBytes+128 || json.Unmarshal([]byte(raw), &s) != nil {
		return AcceptanceSnapshot{State: "malformed"}
	}
	switch s.State {
	case "missing", "malformed":
		s.Block = ""
	case "valid":
		if _, ok := decodeAcceptance(s.Block); !ok {
			return AcceptanceSnapshot{State: "malformed"}
		}
	default:
		return AcceptanceSnapshot{State: "malformed"}
	}
	return s
}

func StoreAcceptanceSnapshot(metadata map[string]string, s AcceptanceSnapshot) {
	raw, _ := json.Marshal(s)
	metadata[AcceptanceEvidenceKey] = string(raw)
}

func AcceptanceEvidenceWarnings(criteria []string, snapshot AcceptanceSnapshot) []AcceptanceWarning {
	reasons := make([]string, len(criteria))
	for i := range reasons {
		reasons[i] = "unmentioned"
	}
	if snapshot.State == "malformed" {
		for i := range reasons {
			reasons[i] = "malformed"
		}
	}
	if snapshot.State == "valid" {
		m, ok := decodeAcceptance(snapshot.Block)
		if !ok {
			return AcceptanceEvidenceWarnings(criteria, AcceptanceSnapshot{State: "malformed"})
		}
		seen := make(map[int]bool)
		for _, raw := range m.Criteria {
			var identity struct {
				Index int `json:"index"`
			}
			if json.Unmarshal(raw, &identity) != nil || identity.Index < 1 || identity.Index > len(criteria) {
				continue
			}
			i := identity.Index - 1
			var entry acceptanceEntry
			if seen[i] || json.Unmarshal(raw, &entry) != nil || strings.TrimSpace(entry.Criterion) != strings.TrimSpace(criteria[i]) || (entry.Status != "met" && entry.Status != "unmet") || (entry.Status == "met" && strings.TrimSpace(entry.Evidence) == "") {
				reasons[i] = "malformed"
			} else if entry.Status == "unmet" {
				reasons[i] = "unmet"
			} else {
				reasons[i] = ""
			}
			seen[i] = true
		}
	}
	var warnings []AcceptanceWarning
	for i, reason := range reasons {
		if reason != "" {
			warnings = append(warnings, AcceptanceWarning{Index: i + 1, Criterion: criteria[i], Reason: reason})
		}
	}
	return warnings
}

func AcceptanceEvidenceNotice(warnings []AcceptanceWarning) string {
	if len(warnings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Warning: acceptance evidence gaps (advisory; submission proceeds):\n")
	for _, w := range warnings {
		fmt.Fprintf(&b, "  %d. %s (%s)\n", w.Index, EscapeDiagnostic(w.Criterion), EscapeDiagnostic(w.Reason))
	}
	return b.String()
}

// RestoreAcceptanceEvidence restores the bounded block after report reconstruction.
func RestoreAcceptanceEvidence(body string, metadata map[string]string) string {
	s := AcceptanceSnapshotFromMetadata(metadata)
	block := AcceptanceEvidenceStart + s.Block + AcceptanceEvidenceEnd
	if s.State == "valid" && !strings.Contains(body, block) {
		// Keep the mapping in the report, ahead of the review-only appendix.
		if i := strings.Index(body, "\n<!-- swarm:guidance:start -->\n"); i >= 0 {
			return body[:i] + "\n" + block + "\n" + body[i:]
		}
		return body + "\n" + block + "\n"
	}
	return body
}
