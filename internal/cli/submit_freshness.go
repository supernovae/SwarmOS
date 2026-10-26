package cli

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/workspace"
)

type submissionFreshness struct {
	Status          string   `json:"status"`
	BaseDrift       *bool    `json:"base_drift"`
	BehindCount     *int     `json:"behind_count"`
	ConflictedPaths []string `json:"conflicted_paths"`
}

// Fetch refreshes only the selected remote-tracking ref. Merge inspection may
// write loose objects, but changes no branch refs, index or worktree.
func checkSubmissionFreshness(ctx context.Context, root, branch, base, head string) submissionFreshness {
	return checkSubmissionFreshnessFrom(ctx, root, workspace.DefaultUpstreamRemote, branch, base, head)
}

// checkSubmissionFreshnessFrom compares against the named upstream remote's
// target branch, never a fork's copy of it.
func checkSubmissionFreshnessFrom(ctx context.Context, root, upstreamRemote, branch, base, head string) submissionFreshness {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f := submissionFreshness{Status: "skipped", ConflictedPaths: []string{}}
	ref := "refs/heads/" + branch
	if hasSubmissionRemote(ctx, root, upstreamRemote) {
		ref = "refs/remotes/" + upstreamRemote + "/" + branch
		if _, err := gitcredential.Output(ctx, root, "fetch", "--no-tags", "--no-write-fetch-head", upstreamRemote, "+refs/heads/"+branch+":"+ref); err != nil {
			return f
		}
	}
	out, err := gitcredential.Output(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return f
	}
	tip := strings.TrimSpace(string(out))
	drift := tip != base
	f.BaseDrift = &drift
	if !drift {
		n := 0
		f.BehindCount = &n
		f.Status = "unchanged"
		return f
	}
	out, err = gitcredential.Output(ctx, root, "rev-list", "--count", base+".."+tip)
	if err != nil {
		return f
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return f
	}
	f.BehindCount = &n
	cmd, _, err := gitcredential.Command(ctx, root, "merge-tree", "--write-tree", "--name-only", "-z", "--no-messages", tip, head)
	if err != nil {
		return f
	}
	out, err = cmd.Output()
	code := 0
	if err != nil {
		e, ok := err.(*exec.ExitError)
		if !ok || e.ExitCode() != 1 {
			return f
		}
		code = 1
	}
	paths, err := parseSubmissionConflicts(out, code)
	if err != nil {
		return f
	}
	f.ConflictedPaths = paths
	f.Status = "clean"
	if code == 1 {
		f.Status = "conflicts"
	}
	return f
}

func parseSubmissionConflicts(out []byte, code int) ([]string, error) {
	records := strings.Split(string(out), "\x00")
	if len(records) < 2 || records[len(records)-1] != "" {
		return nil, fmt.Errorf("invalid merge result")
	}
	tree := records[0]
	if len(tree) != 40 && len(tree) != 64 {
		return nil, fmt.Errorf("invalid tree")
	}
	for _, c := range tree {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return nil, fmt.Errorf("invalid tree")
		}
	}
	seen := map[string]bool{}
	for _, p := range records[1 : len(records)-1] {
		if p == "" {
			return nil, fmt.Errorf("invalid path")
		}
		seen[p] = true
	}
	paths := []string{}
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if (code == 0 && len(paths) > 0) || (code == 1 && len(paths) == 0) {
		return nil, fmt.Errorf("inconsistent result")
	}
	return paths, nil
}

func (f submissionFreshness) advisory() string {
	if f.Status == "skipped" {
		return "Base freshness check skipped: target fetch or merge inspection unavailable.\n"
	}
	if f.Status == "unchanged" {
		return ""
	}
	text := "Target differs from the approved base (0 new reachable commits).\n"
	if f.BehindCount != nil && *f.BehindCount > 0 {
		text = fmt.Sprintf("Base is behind the target by %d commits (observed at submit time).\n", *f.BehindCount)
	}
	if f.Status == "clean" {
		return text + "The validated checkpoint merges cleanly with the observed target.\n"
	}
	return text + "Merge inspection found conflicts; submission will proceed.\n" + f.conflictPaths()
}

func (f submissionFreshness) conflictPaths() string {
	var b strings.Builder
	for _, p := range f.ConflictedPaths {
		fmt.Fprintf(&b, "- %s\n", strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "&#96;", "*", "&#42;", "_", "&#95;", "[", "&#91;", "]", "&#93;", "\\", "&#92;").Replace(strconv.QuoteToASCII(p)))
	}
	return b.String()
}

func submissionRecoveryCommand(pr int, opts *RootOptions) string {
	command := "swarm"
	if remote := strings.TrimSpace(opts.UpstreamRemote); remote != "" {
		command += " --upstream-remote " + submissionShellWord(remote)
	}
	if opts.Fork {
		command += " --fork"
		defaults := *opts
		defaults.ForkRemote = ""
		if remote := opts.effectiveForkRemote(); remote != defaults.effectiveForkRemote() {
			command += " --fork-remote " + submissionShellWord(remote)
		}
	}
	return command + fmt.Sprintf(" pr rebase %d --resolve", pr)
}

func (f submissionFreshness) conflictNote(pr, issue int, opts *RootOptions) string {
	if f.Status != "conflicts" {
		return ""
	}
	command := submissionRecoveryCommand(pr, opts)
	var b strings.Builder
	b.WriteString("\nBase freshness: merge conflicts in:\n")
	b.WriteString(f.conflictPaths())
	fmt.Fprintf(&b, "Run `%s`.\nThe moved base needs approval re-pinned before or during rebase; fork contributors need a maintainer to run `swarm issue approve %d` upstream first.\n", command, issue)
	return b.String()
}

func hasSubmissionRemote(ctx context.Context, root, remote string) bool {
	if workspace.ValidateRemoteName(remote) != nil {
		return false
	}
	_, err := gitcredential.Output(ctx, root, "remote", "get-url", remote)
	return err == nil
}
func submissionShellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
