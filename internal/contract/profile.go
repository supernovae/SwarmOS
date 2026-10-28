package contract

import (
	"fmt"
	"regexp"
)

// ProfileMatch describes approved task dimensions; empty fields are wildcards.
type ProfileMatch struct {
	Size        string `yaml:"size,omitempty" json:"size,omitempty"`
	Uncertainty string `yaml:"uncertainty,omitempty" json:"uncertainty,omitempty"`
	Risk        string `yaml:"risk,omitempty" json:"risk,omitempty"`
}

// ProfileSpec controls prompts and evidence expectations only.
type ProfileSpec struct {
	Name          string       `yaml:"name" json:"name"`
	Match         ProfileMatch `yaml:"match" json:"match"`
	PromptVariant string       `yaml:"prompt_variant" json:"prompt_variant"`
	Evidence      []string     `yaml:"evidence,omitempty" json:"evidence,omitempty"`
}

var profileName = regexp.MustCompile(`^[a-z0-9-]+$`)

func ValidateProfiles(profiles []ProfileSpec) error {
	seen := map[string]bool{}
	for _, p := range profiles {
		if !profileName.MatchString(p.Name) || seen[p.Name] {
			return fmt.Errorf("invalid or duplicate profile name %q", p.Name)
		}
		seen[p.Name] = true
		if !profileName.MatchString(p.PromptVariant) {
			return fmt.Errorf("invalid prompt variant %q", p.PromptVariant)
		}
		for _, v := range []struct {
			value   string
			allowed []string
		}{{p.Match.Size, []string{"", "small", "multi_area"}}, {p.Match.Uncertainty, []string{"", "known", "unknown"}}, {p.Match.Risk, []string{"", "standard", "sensitive", "urgent"}}} {
			ok := false
			for _, a := range v.allowed {
				if a == v.value {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("invalid profile dimension %q", v.value)
			}
		}
	}
	return nil
}

// SelectProfile returns the first full match; unassessed tasks keep legacy prompts.
func SelectProfile(wu *WorkUnit, profiles []ProfileSpec) *ProfileSpec {
	if wu == nil || (wu.Size == "" && wu.Uncertainty == "" && wu.Risk == "") {
		return nil
	}
	for i := range profiles {
		m := profiles[i].Match
		if (m.Size == "" || m.Size == wu.Size) && (m.Uncertainty == "" || m.Uncertainty == wu.Uncertainty) && (m.Risk == "" || m.Risk == wu.Risk) {
			return &profiles[i]
		}
	}
	return nil
}
