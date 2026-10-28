package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func dimensionTask() *WorkUnit {
	return &WorkUnit{ID: "ISSUE-1", IssueNumber: 1, Title: "Test task", Objective: "Implement the feature", Constraints: []string{"Stay scoped"}, Acceptance: []string{"Tests pass"}, Workflow: "feature", Gate: GateHuman}
}

func TestSelectProfile(t *testing.T) {
	ps := []ProfileSpec{{Name: "partial", Match: ProfileMatch{Size: "small", Risk: "sensitive"}}, {Name: "first", Match: ProfileMatch{Size: "small"}}, {Name: "second", Match: ProfileMatch{Size: "small"}}}
	if SelectProfile(dimensionTask(), ps) != nil {
		t.Fatal("unassessed task selected profile")
	}
	wu := dimensionTask()
	wu.Size = "small"
	if got := SelectProfile(wu, ps); got == nil || got.Name != "first" {
		t.Fatalf("selection: %+v", got)
	}
	wu.Risk = "sensitive"
	if SelectProfile(wu, ps).Name != "partial" {
		t.Fatal("full match not selected")
	}
}

func TestValidateProfiles(t *testing.T) {
	good := ProfileSpec{Name: "compact", PromptVariant: "compact", Match: ProfileMatch{Size: "small"}}
	if err := ValidateProfiles([]ProfileSpec{good}); err != nil {
		t.Fatal(err)
	}
	for _, ps := range [][]ProfileSpec{{good, good}, {{Name: "x", PromptVariant: "../x"}}, {{Name: "x", PromptVariant: "x", Match: ProfileMatch{Risk: "critical"}}}, {{Name: "x", PromptVariant: "x", Match: ProfileMatch{Size: "huge"}}}, {{Name: "x", PromptVariant: "x", Match: ProfileMatch{Uncertainty: "maybe"}}}} {
		if ValidateProfiles(ps) == nil {
			t.Fatalf("accepted %+v", ps)
		}
	}
}

func TestWorkUnitDimensions(t *testing.T) {
	a, err := os.ReadFile("workunit.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../.swarm/contracts/workunit.schema.json")
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("schema copies differ", err)
	}
	base := dimensionTask()
	body := FormatIssueMarkdown(base)
	if body != "### Objective\n\nImplement the feature\n\n### Constraints\n\n- Stay scoped\n\n### Acceptance Criteria\n\n- [ ] Tests pass\n\n### Workflow\n\nfeature\n\n### Plan Gate\n\nhuman\n" {
		t.Fatal("legacy markdown changed", body)
	}
	parsedEmpty, err := ParseWorkUnitFromIssue(1, base.Title, body+"\n### Size\n\n_No response_\n\n### Uncertainty\n\n_No response_\n\n### Risk\n\n_No response_\n")
	if err != nil || DigestWorkUnit(parsedEmpty) != DigestWorkUnit(base) {
		t.Fatal("empty form fields", err)
	}
	digest := DigestWorkUnit(base)
	if digest != "a69eac4968e18afa2d7c102a35b125a8bf90d7c8345a147f3ebe430daf26e90f" {
		t.Fatal("legacy task digest changed", digest)
	}
	// Unset dimensions must preserve the pre-dimension JSON digest input.
	raw, _ := json.Marshal(base)
	if strings.Contains(string(raw), "size") || strings.Contains(string(raw), "uncertainty") || strings.Contains(string(raw), "risk") {
		t.Fatal("empty dimensions changed JSON")
	}
	for _, tc := range []struct {
		field   string
		valid   []string
		invalid string
	}{{"size", []string{"small", "multi_area"}, "huge"}, {"uncertainty", []string{"known", "unknown"}, "maybe"}, {"risk", []string{"standard", "sensitive", "urgent"}, "critical"}} {
		for _, v := range append(tc.valid, tc.invalid) {
			wu := *base
			switch tc.field {
			case "size":
				wu.Size = v
			case "uncertainty":
				wu.Uncertainty = v
			case "risk":
				wu.Risk = v
			}
			if err := ValidateWorkUnit(&wu); (err != nil) != (v == tc.invalid) {
				t.Fatalf("%s=%s: %v", tc.field, v, err)
			}
			if v == tc.invalid {
				continue
			}
			if DigestWorkUnit(&wu) == digest {
				t.Fatal("digest unchanged")
			}
			parsed, err := ParseWorkUnitFromIssue(1, wu.Title, FormatIssueMarkdown(&wu))
			if err != nil || DigestWorkUnit(parsed) != DigestWorkUnit(&wu) {
				t.Fatalf("round trip %+v %v", parsed, err)
			}
			for _, source := range []string{body, "```yaml swarm-workunit\n" + string(mustYAML(t, base)) + "```\n"} {
				e := WorkUnitEdit{}
				switch tc.field {
				case "size":
					e.Size = &v
				case "uncertainty":
					e.Uncertainty = &v
				case "risk":
					e.Risk = &v
				}
				_, changed, err := EditWorkUnitIssue(1, base.Title, source, e)
				if err != nil {
					t.Fatal(err)
				}
				empty := ""
				switch tc.field {
				case "size":
					e.Size = &empty
				case "uncertainty":
					e.Uncertainty = &empty
				case "risk":
					e.Risk = &empty
				}
				_, cleared, err := EditWorkUnitIssue(1, base.Title, changed, e)
				if err != nil {
					t.Fatal(err)
				}
				got, err := ParseWorkUnitFromIssue(1, base.Title, cleared)
				if err != nil || DigestWorkUnit(got) != digest {
					t.Fatalf("clear %s: %v %+v", tc.field, err, got)
				}
			}
		}
	}
	if FormatIssueMarkdown(base) != body {
		t.Fatal("legacy body changed")
	}
}

func mustYAML(t *testing.T, v any) []byte {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEditDimensionsPreservesUnselectedContent(t *testing.T) {
	base := dimensionTask()
	for _, newline := range []string{"\n", "\r\n"} {
		body := strings.ReplaceAll(FormatIssueMarkdown(base)+"\n  ### Size  \n\nsmall\n\n### Notes\n\nKeep these bytes.\n", "\n", newline)
		size := "multi_area"
		_, changed, err := EditWorkUnitIssue(1, base.Title, body, WorkUnitEdit{Size: &size})
		if err != nil {
			t.Fatal(err)
		}
		prefix := strings.Split(body, "  ### Size")[0]
		suffix := "### Notes" + newline + newline + "Keep these bytes." + newline
		if !strings.HasPrefix(changed, prefix) || !strings.HasSuffix(changed, suffix) {
			t.Fatal("unselected bytes changed", changed)
		}
		empty := ""
		_, cleared, err := EditWorkUnitIssue(1, base.Title, changed, WorkUnitEdit{Size: &empty})
		if err != nil {
			t.Fatal(err)
		}
		if cleared != prefix+suffix {
			t.Fatalf("clear altered content: %q", cleared)
		}
		invalid := "huge"
		if _, _, err := EditWorkUnitIssue(1, base.Title, body, WorkUnitEdit{Size: &invalid}); err == nil {
			t.Fatal("invalid edit accepted")
		}
	}
}
