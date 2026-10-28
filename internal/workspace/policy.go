package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/supernovae/swarmos/internal/gitcredential"
)

// PolicyTreeDigest returns the committed .swarm tree identity at an immutable
// commit. It deliberately does not inspect the checkout or parse configuration.
func PolicyTreeDigest(ctx context.Context, root, base string) (string, error) {
	if err := prCommit(ctx, root, base); err != nil {
		return "", err
	}
	out, err := gitOutputContext(ctx, root, "rev-parse", "--verify", base+":.swarm")
	if err != nil {
		return "", fmt.Errorf("resolve approved policy tree: %w", err)
	}
	id := strings.TrimSpace(out)
	kind, err := gitOutputContext(ctx, root, "cat-file", "-t", id)
	if err != nil || strings.TrimSpace(kind) != "tree" {
		return "", fmt.Errorf("approved .swarm entry is not a Git tree")
	}
	return id, nil
}

// BaseIsAncestor distinguishes a negative ancestry result from a Git failure.
func BaseIsAncestor(ctx context.Context, root, oldBase, newBase string) (bool, error) {
	for _, base := range []string{oldBase, newBase} {
		if err := prCommit(ctx, root, base); err != nil {
			return false, err
		}
	}
	cmd, redact, err := gitcredential.Command(ctx, root, "merge-base", "--is-ancestor", oldBase, newBase)
	if err != nil {
		return false, err
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check approved base ancestry: %w: %s", err, redact(normalizeGitOutput(string(out))))
}

// ChangedPolicyPaths reports literal repository-relative paths, including both
// sides of renames. NUL separation preserves filenames containing newlines.
func ChangedPolicyPaths(ctx context.Context, root, oldBase, newBase string) ([]string, error) {
	for _, base := range []string{oldBase, newBase} {
		if err := prCommit(ctx, root, base); err != nil {
			return nil, err
		}
	}
	out, err := gitOutputContext(ctx, root, "diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", oldBase, newBase, "--", ".swarm")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// HarnessPolicyWarning describes observed policy drift; it grants no authority.
type HarnessPolicyWarning struct {
	Reason               string   `json:"reason"`
	IssueNumber          int      `json:"issue_number"`
	PRNumber             int      `json:"pr_number,omitempty"`
	ApprovedPolicyDigest string   `json:"approved_policy_digest"`
	TargetPolicyDigest   string   `json:"target_policy_digest"`
	TargetSHA            string   `json:"target_sha"`
	RecoveryCommands     []string `json:"recovery_commands"`
}

// ObserveHarnessPolicy compares committed trees using the same identities as CI.
func ObserveHarnessPolicy(ctx context.Context, root, base, recorded, target string) (*HarnessPolicyWarning, error) {
	approved, err := PolicyTreeDigest(ctx, root, base)
	if err != nil {
		return nil, err
	}
	current, err := PolicyTreeDigest(ctx, root, target)
	if err != nil {
		return nil, err
	}
	if current == approved && (recorded == "" || current == recorded) {
		return nil, nil
	}
	if recorded != "" && recorded != current {
		approved = recorded
	}
	return &HarnessPolicyWarning{Reason: "target_policy_changed_since_approval", ApprovedPolicyDigest: approved, TargetPolicyDigest: current, TargetSHA: target}, nil
}
