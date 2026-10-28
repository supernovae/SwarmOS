package pr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
)

const rebasePlanTask = `Write only .swarm/active/REBASE_PLAN.md. Read CONFLICTS.md, each conflict's base/ours/theirs, and PLAN.md. For every conflicted path write a section headed "## <path>" with nonempty lines "Ours: ...", "Theirs: ...", "Resolution: ...", and "Approved behavior: ..." describing what each side changed, the intended resolution, and how it preserves the approved plan. Do not resolve files yet. Do not run Git.`

type planFile struct {
	data []byte
	mode fs.FileMode
}

// snapshotPlanTree includes control files: a planning turn may write just its
// output artifact, never change the coordinator's context or source inputs.
func snapshotPlanTree(dir string) (map[string]planFile, error) {
	files := map[string]planFile{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe planning file %s", rel)
		}
		if info.Size() > 32<<20 {
			return fmt.Errorf("planning or repair file exceeds snapshot limit: %s", rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = planFile{data, info.Mode()}
		return nil
	})
	return files, err
}

func verifyRebasePlan(dir string, before map[string]planFile, paths []string) (string, error) {
	after, err := snapshotPlanTree(dir)
	if err != nil {
		return "", err
	}
	if err := verifySnapshotDirectories(dir, before, nil, nil); err != nil {
		return "", err
	}
	const output = ".swarm/active/REBASE_PLAN.md"
	for p, a := range after {
		if p == output {
			continue
		}
		b, ok := before[p]
		if !ok || a.mode != b.mode || !bytes.Equal(a.data, b.data) {
			return "", fmt.Errorf("planning turn changed %s", p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			return "", fmt.Errorf("planning turn removed %s", p)
		}
	}
	body := string(after[output].data)
	if len(body) > 64<<10 {
		return "", fmt.Errorf("oversized REBASE_PLAN.md")
	}
	if len(body) == 0 {
		return "", &microPlanError{"missing REBASE_PLAN.md"}
	}
	for _, p := range paths {
		heading := "## " + p + "\n"
		_, section, ok := strings.Cut(body, heading)
		if !ok {
			return "", &microPlanError{fmt.Sprintf("rebase plan omits %s", p)}
		}
		section, _, _ = strings.Cut(section, "\n## ")
		filled := microPlanLabels(section)
		for _, label := range microPlanLabelNames {
			if !filled[label] {
				return "", &microPlanError{fmt.Sprintf("rebase plan lacks %s: for %s", label, p)}
			}
		}
	}
	return body, nil
}

// microPlanError marks a plan rejected only for its form: a missing plan, an
// omitted path section, or an empty label. One automatic retry may fix it.
type microPlanError struct{ msg string }

func (e *microPlanError) Error() string { return e.msg }

var microPlanLabelNames = []string{"Ours", "Theirs", "Resolution", "Approved behavior"}

// microPlanLabels reports which labels in one path section carry text. A
// label may be bold or a list item, and its text may continue on the lines
// after it up to the next label or heading.
func microPlanLabels(section string) map[string]bool {
	filled := map[string]bool{}
	current := ""
	for _, line := range strings.Split(section, "\n") {
		text := line
		if label, rest, ok := microPlanLabel(line); ok {
			current, text = label, rest
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			current = ""
		}
		if current != "" && strings.Trim(text, " \t\r*_-") != "" {
			filled[current] = true
		}
	}
	return filled
}

// microPlanLabel parses "Label: text", "**Label:** text", "**Label**: text",
// and the same after one "-", "*", "+", or "1." list marker.
func microPlanLabel(line string) (label, rest string, ok bool) {
	line = strings.TrimSpace(line)
	if len(line) > 1 && strings.ContainsRune("-*+", rune(line[0])) && line[1] == ' ' {
		line = line[2:]
	} else if digits := len(line) - len(strings.TrimLeft(line, "0123456789")); digits > 0 && len(line) > digits+1 && (line[digits] == '.' || line[digits] == ')') && line[digits+1] == ' ' {
		line = line[digits+2:]
	}
	line = strings.TrimSpace(line)
	emphasis := ""
	for _, e := range []string{"**", "__"} {
		if strings.HasPrefix(line, e) {
			emphasis, line = e, line[len(e):]
			break
		}
	}
	for _, name := range microPlanLabelNames {
		if len(line) < len(name) || !strings.EqualFold(line[:len(name)], name) {
			continue
		}
		after := line[len(name):]
		if emphasis != "" {
			if r, found := strings.CutPrefix(after, emphasis+":"); found {
				return name, r, true
			}
			if r, found := strings.CutPrefix(after, ":"+emphasis); found {
				return name, r, true
			}
		}
		if r, found := strings.CutPrefix(after, ":"); found {
			return name, r, true
		}
	}
	return "", "", false
}

func evidenceDigest(body string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(body))) }

func (s *Service) planRebaseStop(ctx context.Context, rv *resolver, dir string, issue int, owner, stopped string, paths []string, st *lease.IssueState) (string, error) {
	if err := safefs.WriteFile(dir, ".swarm/active/SYSTEM_PROMPT.md", []byte("# Rebase planning\n\n"+rebasePlanTask), 0644); err != nil {
		return "", err
	}
	before, err := snapshotPlanTree(dir)
	if err != nil {
		return "", err
	}
	run, body, err := s.runRebasePlan(ctx, rv, dir, before, paths, rebasePlanTask)
	var malformed *microPlanError
	if errors.As(err, &malformed) && !s.Fork {
		// Only the plan's form was wrong; give the agent one more turn with the
		// reason. The rejected plan is removed so it cannot pass a second time.
		rv.logf("Rebase plan rejected (%s); retrying the planning turn once", malformed)
		safe, openErr := safefs.Open(dir)
		if openErr != nil {
			return "", openErr
		}
		removeErr := safe.Remove(".swarm/active/REBASE_PLAN.md")
		safe.Close()
		if removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return "", removeErr
		}
		retry := rebasePlanTask + "\nThe previous REBASE_PLAN.md was rejected: " + malformed.Error() + ". Rewrite it with every required label."
		run, body, err = s.runRebasePlan(ctx, rv, dir, before, paths, retry)
	}
	if err != nil {
		return "", err
	}
	if st.PRRevision == nil || s.Manager == nil {
		return "", fmt.Errorf("%w: missing fenced rebase context", ErrResolveRejected)
	}
	design := ""
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign {
			design = evidenceDigest(a.Body)
		}
	}
	att := rv.runner.Provenance("developer", false)
	if run != nil && run.Attestation.Agent != "" {
		att = run.Attestation
	}
	report := &lease.RebasePlanReport{Harness: att.Harness, HarnessVersion: att.HarnessVersion, Model: att.Model, Sandbox: att.Sandbox, Commit: stopped, Paths: paths, Body: body, Digest: evidenceDigest(body), PlanDigest: design, BaseSHA: st.BaseSHA, Agent: rv.agentName, Guidance: runGuidance(run)}
	updated, err := s.Manager.UpdateRebaseContext(ctx, issue, owner, st.PRRevision.Number, lease.RebaseContext{Status: "in-progress", Plan: report})
	if err != nil {
		return "", err
	}
	var accumulated strings.Builder
	for _, p := range updated.LastRebase.Plans {
		fmt.Fprintf(&accumulated, "# Stop %s\n\n%s\n", p.Commit, p.Body)
	}
	safe, err := safefs.Open(dir)
	if err != nil {
		return "", err
	}
	defer safe.Close()
	if err := safe.WriteFile(".swarm/active/REBASE_PLAN.md", []byte(accumulated.String()), 0644); err != nil {
		return "", err
	}
	if err := safe.WriteFile(".swarm/active/SYSTEM_PROMPT.md", []byte(resolverSystemPrompt), 0644); err != nil {
		return "", err
	}
	return accumulated.String(), nil
}

// runRebasePlan runs one planning turn and verifies its REBASE_PLAN.md.
func (s *Service) runRebasePlan(ctx context.Context, rv *resolver, dir string, before map[string]planFile, paths []string, task string) (*agent.RunResult, string, error) {
	run, err := rv.runner.Run(ctx, rv.request(dir, task))
	if err != nil {
		return nil, "", fmt.Errorf("%w: planning turn failed: %s", ErrResolveRejected, strings.ReplaceAll(err.Error(), dir, "<resolver workspace>"))
	}
	body, err := verifyRebasePlan(dir, before, paths)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrResolveRejected, err)
	}
	return run, body, nil
}

// Empty directories are output too: reject directory creation outside the
// original snapshot or the parents needed for an allowed repair file.
func verifySnapshotDirectories(dir string, before map[string]planFile, paths []string, rv *resolver) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		for name := range before {
			if strings.HasPrefix(name, rel+"/") {
				return nil
			}
		}
		if rv != nil {
			for _, name := range paths {
				if strings.HasPrefix(name, rel+"/") && repairAllowed(name, paths, rv) {
					return nil
				}
			}
		}
		return fmt.Errorf("agent created directory %s outside the snapshot scope", rel)
	})
}
