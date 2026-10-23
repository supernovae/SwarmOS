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
// of the same template, or --force is set; customized files are kept.
type managedWriter struct {
	safe     *safefs.Root
	manifest *manifest
	force    bool
	res      *InitResult
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
		w.res.SkippedFiles = append(w.res.SkippedFiles, rel)
		return nil
	}
	if err := w.safe.WriteFile(rel, data, 0o644); err != nil {
		return err
	}
	w.manifest.Files[rel] = digest(data)
	if exists {
		w.res.UpdatedFiles = append(w.res.UpdatedFiles, rel)
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
