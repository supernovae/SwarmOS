package pr

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	if len(body) == 0 || len(body) > 64<<10 {
		return "", fmt.Errorf("missing or oversized REBASE_PLAN.md")
	}
	for _, p := range paths {
		heading := "## " + p + "\n"
		_, section, ok := strings.Cut(body, heading)
		if !ok {
			return "", fmt.Errorf("rebase plan omits %s", p)
		}
		section, _, _ = strings.Cut(section, "\n## ")
		for _, label := range []string{"Ours:", "Theirs:", "Resolution:", "Approved behavior:"} {
			found := false
			for _, line := range strings.Split(section, "\n") {
				if strings.HasPrefix(line, label) && strings.TrimSpace(strings.TrimPrefix(line, label)) != "" {
					found = true
				}
			}
			if !found {
				return "", fmt.Errorf("rebase plan lacks %s for %s", label, p)
			}
		}
	}
	return body, nil
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
	run, err := rv.runner.Run(ctx, agent.RunRequest{Agent: rv.agentName, Role: "developer", Mode: rv.mode, WorktreePath: dir, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md", TaskPrompt: rebasePlanTask, Local: rv.local, Dependencies: rv.deps})
	if err != nil {
		return "", fmt.Errorf("%w: planning turn failed: %s", ErrResolveRejected, strings.ReplaceAll(err.Error(), dir, "<resolver workspace>"))
	}
	body, err := verifyRebasePlan(dir, before, paths)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolveRejected, err)
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
	report := &lease.RebasePlanReport{Harness: att.Harness, HarnessVersion: att.HarnessVersion, Model: att.Model, Sandbox: att.Sandbox, Commit: stopped, Paths: paths, Body: body, Digest: evidenceDigest(body), PlanDigest: design, BaseSHA: st.BaseSHA, Agent: rv.agentName}
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
