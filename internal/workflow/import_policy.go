package workflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/validation"
)

// importRejection distinguishes a rejected snapshot from a budget interruption.
type importRejection struct {
	ActiveOnly bool // diagnostic only: every rejected path is under the active directory
	OutputOnly bool // rejected solely for writing another role output, independent of recovery support
	Completed  bool
	Paths      []string
	Role       string
	// Deleted marks a run that deleted approved-base files the approved plan
	// does not delete. It is never recoverable.
	Deleted bool
	// Recoverable is set when every rejected path is another role's output, so
	// a maintainer may re-apply the rest of the run with recover-import.
	Recoverable bool
	// Patch is the saved recovery patch path; PatchNote is the fixed reason
	// none was saved. Both are set by the coordinator after rejection.
	Patch, PatchNote string
}

// RecoverableImport lets the sandbox preserve a patch without knowing policy.
func (e *importRejection) RecoverableImport() bool { return e.Recoverable }

func (e *importRejection) ActiveDirectoryImport() bool { return e.ActiveOnly && !e.Deleted }

func (e *importRejection) RoleOutputImport() bool { return e.OutputOnly }

// roleOutputs are the active files some role may write. Another role writing
// one is the only recoverable rejection; approved inputs never are.
var roleOutputs = map[string]bool{
	".swarm/active/IMPLEMENTATION.md": true,
	".swarm/active/VALIDATION.md":     true,
	".swarm/active/REVIEW.md":         true,
}

func (e *importRejection) Error() string {
	if e.Deleted {
		return fmt.Sprintf("rejected agent changes to: %s (unplanned deletion of approved-base files)", strings.Join(e.Paths, ", "))
	}
	return fmt.Sprintf("rejected agent changes to: %s", strings.Join(e.Paths, ", "))
}

// deletionDirective matches one physical PLAN.md line that deletes a path: an
// optional list marker, a delete verb, and the exact path, bare or in
// backticks, followed by whitespace or the end of the line.
var deletionDirective = regexp.MustCompile("^[ \t]*(?:(?:[-*+]|[0-9]+[.)])[ \t]+)?(?i:delete|remove|rm|drop)[ \t]+(?:`([^`\\s]+)`|([^`\\s]+))(?:[ \t]|$)")

// plannedDeletions returns the paths an approved plan explicitly deletes.
// Mentioning a path, or calling it new or modified, never authorizes deletion.
func plannedDeletions(plan string) map[string]bool {
	planned := map[string]bool{}
	for _, line := range strings.Split(plan, "\n") {
		match := deletionDirective.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if match != nil {
			planned[match[1]+match[2]] = true
		}
	}
	return planned
}

// deletionImportPolicy rejects a developer run that deletes files tracked at
// the approved base unless the approved plan deletes them.
func deletionImportPolicy(role string, deleted []string, base map[string]bool, plan string) error {
	if role != "developer" {
		return nil
	}
	planned := plannedDeletions(plan)
	unique := map[string]bool{}
	for _, path := range deleted {
		if base[path] && !planned[path] {
			unique[path] = true
		}
	}
	if len(unique) == 0 {
		return nil
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	return &importRejection{Paths: names, Role: role, Deleted: true}
}

func writableImportPolicy(role string, paths, patterns []string, allowHarness, fork bool) error {
	rejected := map[string]bool{}
	outputOnly := true
	for _, path := range paths {
		before := rejected[path]
		active := path == ".swarm/active" || strings.HasPrefix(path, ".swarm/active/")
		output := path == ".swarm/active/LESSONS.md" && (role == "developer" || role == "tester") || role == "developer" && path == ".swarm/active/IMPLEMENTATION.md" || role == "tester" && path == ".swarm/active/VALIDATION.md"
		if active && !output {
			rejected[path] = true
		}
		if fork && !output && (path == ".swarm" || strings.HasPrefix(path, ".swarm/") || path == ".github/workflows" || strings.HasPrefix(path, ".github/workflows/")) {
			rejected[path] = true
		}
		if !allowHarness {
			for _, pattern := range patterns {
				if validation.MatchProtectedPath(pattern, path) {
					rejected[path] = true
					outputOnly = false
				}
			}
		}
		if rejected[path] && !before && !roleOutputs[path] {
			outputOnly = false
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	activeOnly := true
	names := make([]string, 0, len(rejected))
	for name := range rejected {
		if name != ".swarm/active" && !strings.HasPrefix(name, ".swarm/active/") {
			activeOnly = false
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return &importRejection{Paths: names, Role: role, Recoverable: outputOnly && !fork, OutputOnly: outputOnly, ActiveOnly: activeOnly}
}

// RecoverableRejection reports whether the import policy refuses exactly
// rejected, and only as other roles' outputs. recover-import re-checks a saved
// patch's header with it before claiming anything.
func RecoverableRejection(role string, rejected, patterns []string, allowHarness bool) bool {
	var rejection *importRejection
	if !errors.As(writableImportPolicy(role, rejected, patterns, allowHarness, false), &rejection) || !rejection.Recoverable {
		return false
	}
	unique := map[string]bool{}
	for _, path := range rejected {
		unique[path] = true
	}
	return len(unique) == len(rejection.Paths)
}

// ImportPolicy applies the agent import policy to a recovered change set.
func ImportPolicy(role string, paths, patterns []string, allowHarness bool) error {
	return writableImportPolicy(role, paths, patterns, allowHarness, false)
}

// developerOutputOnly reports whether a developer was refused only the
// tester-owned VALIDATION.md, which usually means results went to the wrong file.
func (e *importRejection) developerOutputOnly() bool {
	if e.Role != "developer" || len(e.Paths) == 0 {
		return false
	}
	for _, path := range e.Paths {
		if path != ".swarm/active/VALIDATION.md" {
			return false
		}
	}
	return true
}

// developerOutputInstructions is shell-safe inside single quotes.
const developerOutputInstructions = "Record notes, test results, and blocked checks in .swarm/active/IMPLEMENTATION.md. Do not change other .swarm/active/ files except LESSONS.md."

// importRejectionReason explains a rejected import and how to rerun the implement phase.
func importRejectionReason(rejection *importRejection, issue, pr int) string {
	rerun := fmt.Sprintf("swarm issue work %d --phase implement", issue)
	if pr != 0 {
		rerun = fmt.Sprintf("swarm pr work %d --phase implement", pr)
	}
	reason := fmt.Sprintf("%s; returned changes were not imported; worktree was left unchanged; rerun with %s", rejection, rerun)
	if rejection.Deleted {
		reason = fmt.Sprintf("%s; returned changes were not imported; worktree was left unchanged; keep or restore approved-base files the approved plan does not delete (the next developer round's .swarm/active/REPAIR.md carries the approved-base contents of any file a checkpoint lacks); an intentional removal needs an approved PLAN.md line such as `- Delete path/to/file.go`; rerun with %s", rejection, rerun)
	}
	if rejection.developerOutputOnly() {
		reason = fmt.Sprintf("%s; returned changes were not imported; worktree was left unchanged; developers record results in .swarm/active/IMPLEMENTATION.md, not %s; rerun with %s --developer-instructions '%s'", rejection, strings.Join(rejection.Paths, ", "), rerun, developerOutputInstructions)
	}
	if rejection.Completed && (rejection.OutputOnly || rejection.ActiveDirectoryImport()) {
		reason = fmt.Sprintf("%s run completed; returned changes were not imported because the role cannot write %s; worktree was left unchanged; rerun with %s", rejection.Role, strings.Join(rejection.Paths, ", "), rerun)
		if rejection.developerOutputOnly() {
			reason += " --developer-instructions '" + developerOutputInstructions + "'"
		}
		if !rejection.OutputOnly {
			reason += " --" + rejection.Role + "-instructions 'Write logs and scratch output to the sandbox temporary directory (TMPDIR), never under the workspace or .swarm/active/. Redirect output to \"$TMPDIR/check.log\".'"
		} else if rejection.Role == "tester" {
			reason += " --tester-instructions 'Record validation results in .swarm/active/VALIDATION.md; leave other roles output files unchanged.'"
		}
	}
	return reason + recoveryReason(rejection, issue, pr)
}

// recoveryReason names only the saved patch's file name: the reason is
// projected onto the issue, so it must not carry a local path.
func recoveryReason(rejection *importRejection, issue, pr int) string {
	switch {
	case rejection.Patch != "":
		name := filepath.Base(rejection.Patch)
		recover := fmt.Sprintf("swarm issue recover-import %d %s", issue, name)
		if pr != 0 {
			recover = fmt.Sprintf("swarm pr recover-import %d %s", pr, name)
		}
		return fmt.Sprintf("; or keep the other changes: recovery patch %s was saved to your state directory; recover with %s", name, recover)
	case rejection.PatchNote != "":
		return "; no recovery patch: " + rejection.PatchNote
	}
	return ""
}
