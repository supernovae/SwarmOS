package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/safefs"
)

// ManifestPath records the files repo init generated, so later runs can update
// files that are still exactly as generated and leave edited files alone.
const ManifestPath = ".swarm/managed.json"

type manifest struct {
	Version      int               `json:"version"`
	SwarmVersion string            `json:"swarm_version,omitempty"`
	Files        map[string]string `json:"files"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func loadManifest(safe *safefs.Root) (*manifest, error) {
	m := &manifest{Version: 1, Files: map[string]string{}}
	data, err := safe.ReadFile(ManifestPath, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestPath, err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m, nil
}

func (m *manifest) save(safe *safefs.Root) error {
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return safe.WriteFile(ManifestPath, append(data, '\n'), 0o644)
}

// managedWriter writes generated files. A file is replaced only when it is
// missing, still matches what SwarmOS last generated, matches a legacy install
// of the same template, or --force is set; customized files are kept. In dry
// run it only reports what it would do, with a diff for each existing file it
// would change or keep.
type managedWriter struct {
	safe     *safefs.Root
	manifest *manifest
	force    bool
	dryRun   bool
	res      *InitResult
}

func (w *managedWriter) recordDiff(rel string, existing, proposed []byte) {
	if !w.dryRun {
		return
	}
	if w.res.Diffs == nil {
		w.res.Diffs = map[string]string{}
	}
	w.res.Diffs[rel] = unifiedDiff(rel, existing, proposed)
}

// write records data as the generated content of rel. legacy, when set,
// recognizes files from installs that predate the manifest.
func (w *managedWriter) write(rel string, data []byte, legacy func(existing []byte) bool) error {
	existing, err := w.safe.ReadFile(rel, 16<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	exists := err == nil
	switch {
	case exists && string(existing) == string(data):
		w.manifest.Files[rel] = digest(data)
		w.res.CurrentFiles = append(w.res.CurrentFiles, rel)
		return nil
	case exists && !w.force && !w.generatedBefore(rel, existing, legacy):
		if _, recorded := w.manifest.Files[rel]; recorded {
			w.res.CustomizedFiles = append(w.res.CustomizedFiles, rel)
		} else {
			w.res.PreservedFiles = append(w.res.PreservedFiles, rel)
		}
		w.recordDiff(rel, existing, data)
		return nil
	}
	if !w.dryRun {
		if err := w.safe.WriteFile(rel, data, 0o644); err != nil {
			return err
		}
	}
	w.manifest.Files[rel] = digest(data)
	if exists {
		w.res.UpdatedFiles = append(w.res.UpdatedFiles, rel)
		w.recordDiff(rel, existing, data)
	} else {
		w.res.CreatedFiles = append(w.res.CreatedFiles, rel)
	}
	return nil
}

func (w *managedWriter) generatedBefore(rel string, existing []byte, legacy func([]byte) bool) bool {
	if recorded, ok := w.manifest.Files[rel]; ok {
		return recorded == digest(existing)
	}
	return legacy != nil && legacy(existing)
}

var workflowPin = regexp.MustCompile(`(?m)^\s*SWARM_VERSION:\s*(\S+)\s*$`)

// ExistingSwarmPin returns the SwarmOS version a repository's generated
// workflows already install, so re-running init from a development build keeps
// that pin instead of dropping CI.
func ExistingSwarmPin(repoRoot string) string {
	safe, err := safefs.Open(repoRoot)
	if err != nil {
		return ""
	}
	defer safe.Close()
	for _, rel := range []string{".github/workflows/swarm-validate.yml", ".github/workflows/swarm-fork-coordinator.yml"} {
		data, err := safe.ReadFile(rel, 1<<20)
		if err != nil {
			continue
		}
		if m := workflowPin.FindSubmatch(data); m != nil && ValidateSwarmVersionPin(string(m[1])) == nil {
			return string(m[1])
		}
	}
	return ""
}

// samePinnedTemplate recognizes a workflow generated from render with an older
// pin: re-rendering with the file's own pin must reproduce it exactly.
func samePinnedTemplate(render func(pin string) []byte) func([]byte) bool {
	return func(existing []byte) bool {
		m := workflowPin.FindSubmatch(existing)
		return m != nil && string(render(string(m[1]))) == string(existing)
	}
}

// Files SwarmOS used to generate but no longer owns. Each is removed only while
// it is still exactly as SwarmOS wrote it; otherwise it is left to the repository.
var retiredFiles = map[string][]string{
	// Repository-wide issue chooser settings belong to the repository owner.
	".github/ISSUE_TEMPLATE/config.yml": {"blank_issues_enabled: true\ncontact_links:\n  - name: SwarmOS Agent & Harness Skills Guide\n    url: https://github.com/supernovae/swarmos\n    about: Learn how SwarmOS turns structured WorkUnit issues into validated Draft PRs\n"},
}

func (w *managedWriter) retire() error {
	paths := make([]string, 0, len(retiredFiles))
	for rel := range retiredFiles {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		recorded, wasManaged := w.manifest.Files[rel]
		delete(w.manifest.Files, rel)
		existing, err := w.safe.ReadFile(rel, 1<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		untouched := wasManaged && recorded == digest(existing)
		for _, legacy := range retiredFiles[rel] {
			untouched = untouched || (!wasManaged && string(existing) == legacy)
		}
		if !untouched {
			continue
		}
		if !w.dryRun {
			if err := w.safe.Remove(rel); err != nil {
				return err
			}
		}
		w.res.RemovedFiles = append(w.res.RemovedFiles, rel)
	}
	return nil
}

const (
	harnessSectionStart = "<!-- swarm:harness:start -->"
	harnessSectionEnd   = "<!-- swarm:harness:end -->"
	// harnessSection points agents at SwarmOS from a repository's own AGENTS.md.
	harnessSection = harnessSectionStart + `
## SwarmOS

This repository uses SwarmOS for contract-driven work. Coding agents: read ` + "`.swarm/SKILLS.md`" + ` for the issue and PR workflow, ` + "`.swarm/goals.md`" + ` and ` + "`.swarm/rules/`" + ` for project rules, and ` + "`.swarm/validation/required-checks.yaml`" + ` for the checks to run. Inside a task worktree, read ` + "`.swarm/active/WORKUNIT.md`" + ` and ` + "`.swarm/active/SYSTEM_PROMPT.md`" + ` first.
` + harnessSectionEnd
)

var harnessSectionPattern = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(harnessSectionStart) + `.*?` + regexp.QuoteMeta(harnessSectionEnd))

// writeAgentsFile creates AGENTS.md from the template, or adds a marked SwarmOS
// section to a repository's own AGENTS.md. Only text between the markers is
// ever rewritten; the rest of the file stays the repository's. rel is the file
// AGENTS.md resolves to, which a repository may link to CLAUDE.md or elsewhere.
func (w *managedWriter) writeAgentsFile(rel string, template []byte) error {
	existing, err := w.safe.ReadFile(rel, 16<<20)
	if errors.Is(err, os.ErrNotExist) {
		return w.write(rel, template, nil)
	}
	if err != nil {
		return err
	}
	if _, recorded := w.manifest.Files[rel]; recorded || string(existing) == string(template) {
		return w.write(rel, template, nil)
	}
	content := string(existing)
	var updated string
	switch {
	case harnessSectionPattern.MatchString(content):
		updated = harnessSectionPattern.ReplaceAllLiteralString(content, harnessSection)
	case strings.Contains(content, ".swarm/SKILLS.md"):
		// Already points agents at SwarmOS (for example, an older generated copy).
		w.res.PreservedFiles = append(w.res.PreservedFiles, rel)
		return nil
	default:
		updated = strings.TrimRight(content, "\n") + "\n\n" + harnessSection + "\n"
	}
	if updated == content {
		w.res.CurrentFiles = append(w.res.CurrentFiles, rel)
		return nil
	}
	w.recordDiff(rel, existing, []byte(updated))
	if !w.dryRun {
		if err := w.safe.WriteFile(rel, []byte(updated), 0o644); err != nil {
			return err
		}
	}
	w.res.SectionFiles = append(w.res.SectionFiles, rel)
	return nil
}
