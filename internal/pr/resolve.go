package pr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

// ErrResolveRejected means the resolver turn produced nothing the coordinator
// could accept; the rebase is still paused exactly as Git left it.
var ErrResolveRejected = errors.New("conflict resolution not accepted")

const resolverSystemPrompt = `# Role: SwarmOS Conflict Resolver

You are resolving merge conflicts from a managed rebase of an existing pull request.

1. Read .swarm/active/CONFLICTS.md. It lists each conflicted file and where to find both sides.
2. Read .swarm/active/WORKUNIT.md and .swarm/active/PLAN.md. The resolved files must keep the behavior the approved plan describes while keeping the new base's changes.
3. Read .swarm/active/REBASE_PLAN.md when present and follow its intended resolutions. Edit only the listed conflicted files. Remove every conflict marker and leave a version that combines both sides' intent.
4. Do not create, delete, rename, or edit any other file outside .swarm/active/. There is no Git repository here; do not try to run Git.

Stop when every listed file is resolved. The coordinator verifies your edits, stages them, continues the rebase, and runs validation separately.
`

const resolverTaskPrompt = "Resolve the conflicts listed in .swarm/active/CONFLICTS.md by editing only those files, following .swarm/active/SYSTEM_PROMPT.md."

// resolver holds what each conflict turn needs; it is built from the approved
// base policy before any claim or rebase mutation.
type resolver struct {
	runner    agent.Runner
	agentName string
	mode      string
	local     bool
	unit      *contract.WorkUnit
	protected []string
	log       io.Writer
	// deps are the agent dependency caches prefetched for the approved base.
	deps sandbox.Dependencies
	// instructions is operator guidance for every resolver turn.
	instructions string
}

// request builds one resolver turn, carrying operator guidance the way issue
// and PR work turns do.
func (r *resolver) request(dir, task string) agent.RunRequest {
	if r.instructions != "" {
		task += "\nOperator instructions: " + r.instructions
	}
	return agent.RunRequest{Agent: r.agentName, Role: "developer", Mode: r.mode, WorktreePath: dir, SystemPrompt: ".swarm/active/SYSTEM_PROMPT.md",
		TaskPrompt: task, OperatorInstructions: r.instructions, Local: r.local, Dependencies: r.deps}
}

// runGuidance is the guidance a turn received, bounded for rebase evidence.
func runGuidance(run *agent.RunResult) []string {
	if run == nil {
		return nil
	}
	return lease.TrimRebaseGuidance(run.Guidance)
}

func (r *resolver) logf(format string, args ...any) {
	if r.log != nil {
		fmt.Fprintf(r.log, format+"\n", args...)
	}
}

func resolveGuidance(number int, paths []string) string {
	var b strings.Builder
	if len(paths) > 0 {
		fmt.Fprintf(&b, "Conflicted paths: %s\n", strings.Join(paths, ", "))
	}
	fmt.Fprintf(&b, "The rebase is still paused. Resolve the files in .swarm/worktrees/pr-%d, git add them, and run swarm pr rebase %d --continue; run swarm pr rebase %d --resolve to retry the agent; or run swarm pr rebase %d --abort.\n", number, number, number, number)
	b.WriteString("Files that contain literal conflict-marker lines (such as marker test fixtures) always need manual resolution.")
	return b.String()
}

// PausedRebaseError explains how to finish or abandon a paused PR rebase.
func PausedRebaseError(ctx context.Context, worktree string, number int) error {
	paths, _ := workspace.RebaseConflicts(ctx, worktree)
	msg := fmt.Sprintf("PR #%d has a paused rebase", number)
	if len(paths) > 0 {
		msg += " with conflicts in " + strings.Join(paths, ", ")
	}
	return &ConflictError{PRNumber: number, Paused: true, Err: fmt.Errorf("%s; run swarm pr rebase %d --continue after staging resolved files, swarm pr rebase %d --resolve, or swarm pr rebase %d --abort", msg, number, number, number)}
}

// resolveStop gives the developer agent one turn on the current rebase stop and
// imports the result only if it is limited to the conflicted files.
func (s *Service) resolveStop(ctx context.Context, rv *resolver, worktree string, issue int, owner string, st *lease.IssueState) (*lease.RebaseResolutionReport, error) {
	stopped, err := workspace.StoppedCommit(ctx, worktree)
	if err != nil {
		return nil, err
	}
	inputs, err := workspace.ConflictInputs(ctx, worktree)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	var paths []string
	for _, in := range inputs {
		if !rv.unit.AllowHarnessChanges {
			for _, pattern := range rv.protected {
				if validation.MatchProtectedPath(pattern, in.Path) {
					return nil, fmt.Errorf("%w: %s is a protected path", workspace.ErrConflictIneligible, in.Path)
				}
			}
		}
		paths = append(paths, in.Path)
	}
	rv.logf("Resolving %d conflicted path(s) at %s with %s", len(paths), shortCommit(stopped), rv.agentName)

	dir, err := os.MkdirTemp("", "swarm-resolve-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	original, err := writeResolverWorkspace(dir, inputs, rv.unit, st)
	if err != nil {
		return nil, err
	}
	plan, err := s.planRebaseStop(ctx, rv, dir, issue, owner, stopped, paths, st)
	if err != nil {
		return nil, err
	}
	if err = safefs.WriteFile(worktree, ".swarm/active/REBASE_PLAN.md", []byte(plan), 0644); err != nil {
		return nil, err
	}
	run, runErr := rv.runner.Run(ctx, rv.request(dir, resolverTaskPrompt))
	if runErr != nil {
		// Never surface the disposable workspace path.
		msg := strings.ReplaceAll(runErr.Error(), dir, "<resolver workspace>")
		return nil, fmt.Errorf("%w: resolver agent %q failed: %s", ErrResolveRejected, rv.agentName, msg)
	}
	resolved, err := verifyResolverOutput(dir, original, paths)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResolveRejected, err)
	}
	if err := s.Manager.CheckClaim(ctx, issue, owner); err != nil {
		return nil, err
	}
	// The stop must be the one the agent saw before anything is written.
	if now, err := workspace.StoppedCommit(ctx, worktree); err != nil || now != stopped {
		return nil, fmt.Errorf("%w: rebase moved during resolution", ErrResolveRejected)
	}
	current, err := workspace.ConflictInputs(ctx, worktree)
	if err != nil {
		return nil, err
	}
	if len(current) != len(inputs) {
		return nil, fmt.Errorf("%w: conflicts changed during resolution", ErrResolveRejected)
	}
	for i := range current {
		if current[i].Path != inputs[i].Path || !bytes.Equal(current[i].Worktree, inputs[i].Worktree) {
			return nil, fmt.Errorf("%w: conflicts changed during resolution", ErrResolveRejected)
		}
	}
	if err := importResolved(ctx, worktree, current, resolved); err != nil {
		return nil, err
	}
	var att contract.RoleAttestation
	if run != nil {
		att = run.Attestation
	}
	if att.Agent == "" {
		att = rv.runner.Provenance("developer", false)
	}
	return &lease.RebaseResolutionReport{Commit: stopped, Paths: paths, Agent: firstNonEmpty(att.Agent, rv.agentName),
		Harness: att.Harness, HarnessVersion: att.HarnessVersion, Model: att.Model, Sandbox: att.Sandbox, Guidance: runGuidance(run)}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// writeResolverWorkspace lays out only the conflict inputs and coordinator
// context. It returns the bytes of every non-control file for verification.
func writeResolverWorkspace(dir string, inputs []workspace.ConflictInput, unit *contract.WorkUnit, st *lease.IssueState) (map[string][]byte, error) {
	root, err := safefs.Open(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	original := map[string][]byte{}
	var manifest strings.Builder
	manifest.WriteString("# Conflicts to resolve\n\nIn a rebase, \"ours\" is the new base the PR is moving onto and \"theirs\" is the PR commit being replayed.\n\n")
	for i, in := range inputs {
		if err := root.WriteFile(filepath.FromSlash(in.Path), in.Worktree, in.Mode); err != nil {
			return nil, err
		}
		original[in.Path] = in.Worktree
		sides := filepath.Join(".swarm", "active", "conflicts", fmt.Sprint(i+1))
		fmt.Fprintf(&manifest, "## %d. `%s`\n\n", i+1, in.Path)
		if in.Base != nil {
			if err := root.WriteFile(filepath.Join(sides, "base"), in.Base.Data, 0o644); err != nil {
				return nil, err
			}
			fmt.Fprintf(&manifest, "- Common ancestor: `.swarm/active/conflicts/%d/base`\n", i+1)
		} else {
			manifest.WriteString("- Common ancestor: none (both sides added this file)\n")
		}
		if err := root.WriteFile(filepath.Join(sides, "ours"), in.Ours.Data, 0o644); err != nil {
			return nil, err
		}
		if err := root.WriteFile(filepath.Join(sides, "theirs"), in.Theirs.Data, 0o644); err != nil {
			return nil, err
		}
		fmt.Fprintf(&manifest, "- New base (ours): `.swarm/active/conflicts/%d/ours`\n- PR commit (theirs): `.swarm/active/conflicts/%d/theirs`\n\n", i+1, i+1)
	}
	plan := "No approved plan is recorded for this task.\n"
	for _, a := range st.Artifacts {
		if a.Type == contract.ArtifactDesign && strings.TrimSpace(a.Body) != "" {
			plan = a.Body
		}
	}
	files := map[string]string{
		"CONFLICTS.md":     manifest.String(),
		"PLAN.md":          plan,
		"SYSTEM_PROMPT.md": resolverSystemPrompt,
		"WORKUNIT.md":      renderResolverWorkUnit(unit),
	}
	for name, body := range files {
		if err := root.WriteFile(filepath.Join(".swarm", "active", name), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	return original, nil
}

func renderResolverWorkUnit(wu *contract.WorkUnit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Work Unit #%d: %s\n\n## Objective\n\n%s\n", wu.IssueNumber, wu.Title, wu.Objective)
	for _, section := range []struct {
		title string
		items []string
	}{{"Constraints", wu.Constraints}, {"Acceptance Criteria", wu.Acceptance}} {
		if len(section.items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", section.title)
		for _, item := range section.items {
			fmt.Fprintf(&b, "- %s\n", item)
		}
	}
	return b.String()
}

func isControlPath(rel string) bool {
	return rel == ".swarm" || rel == ".swarm/active" || strings.HasPrefix(rel, ".swarm/active/")
}

// verifyResolverOutput accepts only edits to the conflicted files: no new or
// removed entries, no links or special files, and no remaining markers.
func verifyResolverOutput(dir string, original map[string][]byte, conflicted []string) (map[string][]byte, error) {
	dirs := map[string]bool{".": true, ".swarm": true}
	for name := range original {
		for d := filepath.ToSlash(filepath.Dir(name)); d != "."; d = filepath.ToSlash(filepath.Dir(d)) {
			dirs[d] = true
		}
	}
	seen := map[string]bool{}
	resolved := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != ".swarm" && isControlPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if !dirs[rel] {
				return fmt.Errorf("resolver created directory %s", rel)
			}
			return nil
		case !info.Mode().IsRegular():
			return fmt.Errorf("resolver created a link or special file at %s", rel)
		}
		before, known := original[rel]
		if !known {
			return fmt.Errorf("resolver created %s", rel)
		}
		seen[rel] = true
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !slices.Contains(conflicted, rel) {
			if !bytes.Equal(data, before) {
				return fmt.Errorf("resolver edited %s, which is not a conflicted path", rel)
			}
			return nil
		}
		if workspace.HasConflictMarkers(data) {
			return fmt.Errorf("conflict markers remain in %s", rel)
		}
		resolved[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name := range original {
		if !seen[name] {
			return nil, fmt.Errorf("resolver removed %s", name)
		}
	}
	return resolved, nil
}

// importResolved writes accepted bytes through safefs and stages them. Any
// failure restores every original file so the paused rebase stays intact.
func importResolved(ctx context.Context, worktree string, inputs []workspace.ConflictInput, resolved map[string][]byte) (err error) {
	root, err := safefs.Open(worktree)
	if err != nil {
		return err
	}
	defer root.Close()
	var written []workspace.ConflictInput
	defer func() {
		if err == nil {
			return
		}
		for _, in := range written {
			if restoreErr := root.WriteFile(filepath.FromSlash(in.Path), in.Worktree, in.Mode); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore %s: %w", in.Path, restoreErr))
			}
		}
	}()
	var paths []string
	for _, in := range inputs {
		data, ok := resolved[in.Path]
		if !ok {
			return fmt.Errorf("%w: no resolution for %s", ErrResolveRejected, in.Path)
		}
		written = append(written, in)
		if err = root.WriteFile(filepath.FromSlash(in.Path), data, in.Mode); err != nil {
			return fmt.Errorf("import %s: %w", in.Path, err)
		}
		paths = append(paths, in.Path)
	}
	return workspace.StageResolved(ctx, worktree, paths)
}
