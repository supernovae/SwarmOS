// Package changelog folds independent change fragments into a release.
package changelog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

var versionToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
var listItem = regexp.MustCompile(`^(?:[-+*](?:\s|$)|[0-9]+[.)](?:\s|$))`)

// Fold validates all fragments before replacing CHANGELOG.md, then removes the
// consumed files. Replacement and deletion are not a multi-file transaction.
func Fold(root, version string) error {
	r, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer r.Close()
	dir, err := r.Sub("changes/unreleased", false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := os.ReadDir(filepath.Join(root, "changes/unreleased"))
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	if !versionToken.MatchString(version) {
		return fmt.Errorf("invalid release version %q: use a single version token", version)
	}
	var bullets []string
	for _, name := range names {
		data, err := dir.ReadFile(name, 4<<20)
		if err != nil {
			return fmt.Errorf("fragment %s: %w", name, err)
		}
		bullet, err := fragment(string(data))
		if err != nil {
			return fmt.Errorf("fragment %s: %w", name, err)
		}
		bullets = append(bullets, bullet)
	}
	data, err := r.ReadFile("CHANGELOG.md", 32<<20)
	if err != nil {
		return err
	}
	text := string(data)
	anchor := "## Unreleased\n"
	if strings.Count(text, anchor) != 1 || !(strings.HasPrefix(text, anchor) || strings.Contains(text, "\n"+anchor)) {
		return fmt.Errorf("CHANGELOG.md needs exactly one ## Unreleased heading")
	}
	end := strings.Index(text, anchor) + len(anchor)
	tail := text[end:]
	// Keep every byte of released history; only blank space may follow the anchor.
	offset := strings.Index(tail, "\n## ")
	suffix := ""
	if offset >= 0 {
		suffix = tail[offset+1:]
		tail = tail[:offset+1]
	}
	if strings.TrimSpace(tail) != "" {
		return fmt.Errorf("unreleased section must be empty; move entries into fragments first")
	}
	output := text[:end] + "\n## " + version + "\n\n" + strings.Join(bullets, "\n\n") + "\n\n" + suffix
	if err := r.WriteFile("CHANGELOG.md", []byte(output), 0644); err != nil {
		return err
	}
	for _, name := range names {
		if err := dir.Remove(name); err != nil {
			return fmt.Errorf("changelog updated but removing fragment %s failed; inspect changes before retrying: %w", name, err)
		}
	}
	return nil
}

func fragment(text string) (string, error) {
	text = strings.Trim(text, " \t\r\n")
	lines := strings.Split(text, "\n")
	if !strings.HasPrefix(text, "- ") || strings.TrimSpace(strings.TrimPrefix(lines[0], "- ")) == "" {
		return "", fmt.Errorf("expected one nonempty '- ' bullet")
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !(strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) || listItem.MatchString(trimmed) || strings.HasPrefix(trimmed, "#") {
			return "", fmt.Errorf("expected only indented prose after the bullet")
		}
	}
	return text, nil
}
