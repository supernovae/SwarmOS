package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/gitcredential"
	"github.com/supernovae/swarmos/internal/harness"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workspace"
)

const harnessGrantVerdict = "granted"

// harnessGrantRecord is what the token-holding CI grant step verified. The
// validate step re-checks it against its own checkout before trusting it.
type harnessGrantRecord struct {
	Verdict          string             `json:"verdict"`
	Issue            int                `json:"issue"`
	PR               int                `json:"pr"`
	Base             string             `json:"base"`
	CandidateHead    string             `json:"candidate_head"`
	ApprovedBase     string             `json:"approved_base"`
	TaskDigest       string             `json:"task_digest"`
	PolicyDigest     string             `json:"policy_digest"`
	PolicyTreeDigest string             `json:"policy_tree_digest"`
	WorkUnit         *contract.WorkUnit `json:"work_unit"`
}

// harnessGrantCandidate resolves the trusted base and the pull request head.
// On GitHub's merge checkout the first parent is the base and the second the
// PR head; otherwise HEAD itself is the candidate.
func harnessGrantCandidate(ctx context.Context, root, base string) (resolvedBase, head string, err error) {
	rev := func(name string) string {
		out, err := gitcredential.Output(ctx, root, "rev-parse", "-q", "--verify", name+"^{commit}")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if strings.HasPrefix(base, "-") {
		return "", "", fmt.Errorf("invalid --base %q", base)
	}
	if resolvedBase = rev(base); resolvedBase == "" {
		return "", "", fmt.Errorf("trusted base %s is unavailable", base)
	}
	if head = rev("HEAD"); head == "" {
		return "", "", fmt.Errorf("candidate HEAD is unavailable")
	}
	if second := rev("HEAD^2"); second != "" && rev("HEAD^1") == resolvedBase {
		head = second
	}
	return resolvedBase, head, nil
}

// resolveHarnessGrant reads the linked issue's authoritative state from the
// ledger namespace chosen by the trusted base and returns the WorkUnit only if
// that state still approves its protected changes for this exact PR head. It
// reads no pull request text, labels, or comments.
func resolveHarnessGrant(ctx context.Context, options *RootOptions, cmd *cobra.Command, root, base string, issue, pr int) (*harnessGrantRecord, error) {
	base, head, err := harnessGrantCandidate(ctx, root, base)
	if err != nil {
		return nil, err
	}
	ns, err := harness.LedgerNamespaceAt(root, base)
	if err != nil {
		return nil, err
	}
	layout, err := coordination.LayoutFor(ns)
	if err != nil {
		return nil, err
	}
	cfg, err := harness.LoadConfigAt(root, base)
	if err != nil {
		return nil, fmt.Errorf("load trusted policy: %w", err)
	}
	tr, err := options.resolveTracker(cmd)
	if err != nil {
		return nil, err
	}
	var store coordination.Store = coordination.NewFileStore(root)
	if strings.HasPrefix(tr.BackendName(), "github") {
		store = coordination.NewGitStoreWithLayout(root, options.effectiveUpstreamRemote(), layout)
	}
	raw, _, err := store.Load(ctx, issue)
	if err != nil {
		return nil, fmt.Errorf("load authoritative state: %w", err)
	}
	var st lease.IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("invalid authoritative state: %w", err)
	}
	_, unit, err := loadWorkUnit(ctx, tr, issue)
	if err != nil {
		return nil, err
	}
	if err := lease.VerifyHarnessGrant(ctx, root, &st, unit, cfg, lease.HarnessGrant{Issue: issue, PR: pr, Base: base, CandidateHead: head}); err != nil {
		return nil, err
	}
	tree, err := workspace.PolicyTreeDigest(ctx, root, base)
	if err != nil {
		return nil, err
	}
	return &harnessGrantRecord{Verdict: harnessGrantVerdict, Issue: issue, PR: pr, Base: base, CandidateHead: head, ApprovedBase: st.BaseSHA,
		TaskDigest: st.TaskDigest, PolicyDigest: st.PolicyDigest, PolicyTreeDigest: tree, WorkUnit: unit}, nil
}

// readHarnessGrantFile returns the WorkUnit of a grant recorded by the CI
// grant step, after re-checking the base, candidate head, task, and policy
// against this checkout. Any mismatch refuses the grant.
func readHarnessGrantFile(ctx context.Context, root, path, base string) (*contract.WorkUnit, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("grant file is not a small regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record harnessGrantRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("invalid grant file: %w", err)
	}
	refuse := func(reason string) (*contract.WorkUnit, error) {
		return nil, fmt.Errorf("%w: %s", lease.ErrHarnessGrant, reason)
	}
	wu := record.WorkUnit
	if record.Verdict != harnessGrantVerdict || wu == nil || !wu.AllowHarnessChanges || wu.IssueNumber != record.Issue || record.Issue <= 0 || record.PR <= 0 {
		return refuse("grant file does not record an approved harness WorkUnit")
	}
	if contract.DigestWorkUnit(wu) != record.TaskDigest {
		return refuse("grant file WorkUnit does not match its task digest")
	}
	base, head, err := harnessGrantCandidate(ctx, root, base)
	if err != nil {
		return nil, err
	}
	if record.Base != base {
		return refuse("grant file was verified for another base")
	}
	if record.CandidateHead != head {
		return refuse("grant file was verified for another candidate head")
	}
	tree, err := workspace.PolicyTreeDigest(ctx, root, base)
	if err != nil || tree != record.PolicyTreeDigest {
		return refuse("committed policy differs from the verified grant")
	}
	contained, err := workspace.BaseIsAncestor(ctx, root, record.ApprovedBase, base)
	if err != nil || !contained {
		return refuse("approved base is not an ancestor of the CI base")
	}
	cfg, err := harness.LoadConfigAt(root, base)
	if err != nil || contract.DigestApprovedPolicy(record.ApprovedBase, cfg) != record.PolicyDigest {
		return refuse("policy digest differs from the verified grant")
	}
	return wu, nil
}

func writeHarnessGrantFile(path string, record *harnessGrantRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func writeHarnessGrantRefusal(w io.Writer, issue int, err error) {
	fmt.Fprintf(w, "NOTE linked issue #%d does not authorize protected changes: %s\n", issue, validation.EscapeDiagnostic(err.Error()))
}

func writeHarnessGrantAccepted(w io.Writer, issue int) {
	fmt.Fprintf(w, "NOTE protected changes authorized by approved WorkUnit #%d (allow_harness_changes)\n", issue)
}
