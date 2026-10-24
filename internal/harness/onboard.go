package harness

import (
	"fmt"
	"strings"
)

// OnboardCommand is the slash command that walks a coding agent through
// onboarding a repository to SwarmOS or upgrading it to a newer swarm.
const OnboardCommand = "swarm-onboard.md"

// OnboardingGuide returns the onboarding guide as a plain prompt, without the
// slash-command front matter, so any harness can follow it before the
// repository has been initialized.
func OnboardingGuide() (string, error) {
	data, err := embeddedTemplates.ReadFile("templates/commands/" + OnboardCommand)
	if err != nil {
		return "", fmt.Errorf("read embedded onboarding guide: %w", err)
	}
	return stripFrontMatter(string(data)), nil
}

// stripFrontMatter drops a leading "---" front matter block. Line endings are
// normalized first, since Windows checkouts may embed templates with CRLF.
func stripFrontMatter(doc string) string {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(doc, "---\n"); ok {
		if _, body, found := strings.Cut(rest, "\n---\n"); found {
			doc = body
		}
	}
	return strings.TrimLeft(doc, "\n")
}
