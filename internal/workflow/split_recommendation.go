package workflow

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

// SplitRecommendationLine is the exact PLAN.md line a planner writes when the
// WorkUnit cannot finish within one claim. It only stops a run after planning;
// it never approves, decomposes, or changes the WorkUnit.
const SplitRecommendationLine = "Split recommendation: required"

// PlanRecommendsSplit reports whether plan has the split recommendation on a
// line of its own (trailing whitespace and CRLF allowed). Coordinator guidance
// is ignored, as are quoted, inline, prefixed, or differently cased mentions.
func PlanRecommendsSplit(plan string) bool {
	for _, line := range strings.Split(canonicalReport(plan), "\n") {
		if strings.TrimRight(line, " \t\r") == SplitRecommendationLine {
			return true
		}
	}
	return false
}

// splitRecommended reports whether the latest recorded design artifact
// recommends a split. Only the fenced artifact counts, never worktree files.
func splitRecommended(st *lease.IssueState) bool {
	if st == nil {
		return false
	}
	for i := len(st.Artifacts) - 1; i >= 0; i-- {
		if st.Artifacts[i].Type == contract.ArtifactDesign {
			return PlanRecommendsSplit(st.Artifacts[i].Body)
		}
	}
	return false
}

// SplitChild is an untrusted planner proposal, not a WorkUnit approval.
type SplitChild struct {
	ID          string   `yaml:"id" json:"id"`
	Title       string   `yaml:"title" json:"title"`
	Objective   string   `yaml:"objective" json:"objective"`
	Acceptance  []string `yaml:"acceptance" json:"acceptance"`
	DependsOn   []string `yaml:"depends_on" json:"depends_on"`
	Size        string   `yaml:"size,omitempty" json:"size,omitempty"`
	Uncertainty string   `yaml:"uncertainty,omitempty" json:"uncertainty,omitempty"`
	Risk        string   `yaml:"risk,omitempty" json:"risk,omitempty"`
	Covers      []int    `yaml:"covers" json:"covers"`
}
type splitProposal struct {
	Version  int              `yaml:"split_proposal_version"`
	Children []SplitChild     `yaml:"children"`
	Coverage map[int][]string `yaml:"coverage,omitempty"`
}

func parseSplitProposal(body string, acceptance int) ([]SplitChild, []int, error) {
	fail := func(err error) ([]SplitChild, []int, error) {
		return nil, nil, fmt.Errorf("invalid split recommendation: %w; record a version 1 YAML split proposal in the design artifact", err)
	}
	body = canonicalReport(body)
	var block string
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if lines[i] != "```yaml" {
			continue
		}
		start := i + 1
		for i++; i < len(lines) && lines[i] != "```"; i++ {
		}
		candidate := strings.Join(lines[start:i], "\n")
		if strings.Contains(candidate, "split_proposal_version:") {
			if block != "" || i == len(lines) {
				return fail(fmt.Errorf("multiple or unterminated proposal blocks"))
			}
			block = candidate
		}
	}
	if block == "" {
		return fail(fmt.Errorf("missing YAML proposal"))
	}
	var p splitProposal
	dec := yaml.NewDecoder(strings.NewReader(block))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return fail(err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fail(fmt.Errorf("proposal must contain one YAML document"))
	}
	if p.Version != 1 || len(p.Children) == 0 {
		return fail(fmt.Errorf("unsupported version or empty children"))
	}
	ids := map[string]int{}
	coverage := map[int]map[string]bool{}
	for i, c := range p.Children {
		if strings.TrimSpace(c.ID) == "" || ids[c.ID] != 0 || strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.Objective) == "" || len(c.Acceptance) == 0 || c.DependsOn == nil || c.Covers == nil {
			return fail(fmt.Errorf("child %q needs unique id, title, objective, acceptance, explicit depends_on and covers", c.ID))
		}
		ids[c.ID] = i + 1
		for _, a := range c.Acceptance {
			if strings.TrimSpace(a) == "" {
				return fail(fmt.Errorf("empty acceptance in %q", c.ID))
			}
		}
		if err := contract.ValidateProfiles([]contract.ProfileSpec{{Name: "child", PromptVariant: "child", Match: contract.ProfileMatch{Size: c.Size, Uncertainty: c.Uncertainty, Risk: c.Risk}}}); err != nil {
			return fail(err)
		}
		for _, n := range c.Covers {
			if n < 1 || n > acceptance {
				return fail(fmt.Errorf("unknown acceptance index %d", n))
			}
			if coverage[n] == nil {
				coverage[n] = map[string]bool{}
			}
			if coverage[n][c.ID] {
				return fail(fmt.Errorf("duplicate coverage for %q", c.ID))
			}
			coverage[n][c.ID] = true
		}
	}
	for n, children := range p.Coverage {
		if n < 1 || n > acceptance {
			return fail(fmt.Errorf("unknown coverage index %d", n))
		}
		seen := map[string]bool{}
		for _, id := range children {
			if ids[id] == 0 || seen[id] || !coverage[n][id] {
				return fail(fmt.Errorf("conflicting coverage mapping for %d", n))
			}
			seen[id] = true
		}
		if len(seen) != len(coverage[n]) {
			return fail(fmt.Errorf("conflicting coverage mapping for %d", n))
		}
	}
	// Stable Kahn order retains proposal order among independent children.
	var sorted []SplitChild
	done := map[string]bool{}
	for len(sorted) < len(p.Children) {
		progress := false
		for _, c := range p.Children {
			if done[c.ID] {
				continue
			}
			ready := true
			seen := map[string]bool{}
			for _, dep := range c.DependsOn {
				if ids[dep] == 0 || seen[dep] {
					return fail(fmt.Errorf("unknown or duplicate dependency %q", dep))
				}
				seen[dep] = true
				ready = ready && done[dep]
			}
			if ready {
				sorted = append(sorted, c)
				done[c.ID] = true
				progress = true
				break
			}
		}
		if !progress {
			return fail(fmt.Errorf("dependency cycle"))
		}
	}
	var uncovered []int
	for n := 1; n <= acceptance; n++ {
		if len(coverage[n]) == 0 {
			uncovered = append(uncovered, n)
		}
	}
	return sorted, uncovered, nil
}
