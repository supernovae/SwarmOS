package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/workspace"
)

func reusableVersion(version string) bool {
	// Pseudo-versions identify immutable builds too; dirty builds do not.
	return version != "" && version != "dev" && !strings.HasSuffix(version, "+dirty")
}

func fingerprint(ctx context.Context, root string, opts ValidateOptions) (map[string]string, error) {
	tree, err := workspace.CheckpointTree(ctx, root)
	if err != nil {
		return nil, err
	}
	policy, err := gitOutput(root, "show", opts.BaseSHA+":.swarm/validation/required-checks.yaml")
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(policy)
	return map[string]string{"validation_tree": tree, "validation_base": opts.BaseSHA, "validation_policy": hex.EncodeToString(digest[:]), "validation_claim": opts.ClaimID, "validation_version": opts.Version}, nil
}

// Reuse returns nil on any mismatch. Only a coordinator-stamped PASS is eligible.
// Explicit Validate and CI never consult this function.
func Reuse(ctx context.Context, root string, opts ValidateOptions, owner string, artifacts []contract.Artifact) (*ValidationReport, error) {
	started := time.Now()
	if opts.ForkMode || opts.CIMode || !reusableVersion(opts.Version) || opts.ClaimID == "" || owner == "" {
		return nil, nil
	}
	if _, err := loadTrustedValidationSpec(root, opts.BaseSHA); err != nil {
		return nil, err
	}
	current, err := fingerprint(ctx, root, opts)
	if err != nil {
		// Optional reuse must not make ordinary validation depend on being
		// able to construct a temporary-index fingerprint.
		return nil, nil
	}
	// Submit publishes the recorded commit, so bind to its actual tree as well
	// as requiring the current candidate to describe that same tree.
	committed, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	if current["validation_tree"] != strings.TrimSpace(string(committed)) {
		return nil, nil
	}
	for i := len(artifacts) - 1; i >= 0; i-- {
		art := artifacts[i]
		if art.Type != contract.ArtifactValidation {
			continue
		}
		if art.Verdict != "PASS" || art.Metadata["validation_trusted_origin"] != "coordinator" || art.Metadata["validation_trusted_owner"] != owner || art.Metadata["validation_trusted_claim"] != opts.ClaimID {
			return nil, nil
		}
		for k, v := range current {
			if v == "" || art.Metadata[k] != v {
				return nil, nil
			}
		}
		var report ValidationReport
		if err := json.Unmarshal([]byte(art.Metadata["validation_report"]), &report); err != nil || !report.Passed {
			return nil, nil
		}
		source := art.Metadata["validation_source_time"]
		if source == "" {
			source = art.CreatedAt.Format(time.RFC3339Nano)
		}
		when, err := time.Parse(time.RFC3339Nano, source)
		if err != nil {
			return nil, nil
		}
		head, err := resolveCommit(root, "HEAD")
		if err != nil {
			return nil, err
		}
		metadata := make(map[string]string, len(art.Metadata)+2)
		for k, v := range art.Metadata {
			metadata[k] = v
		}
		art.Metadata = metadata
		// Reuse may accept the same tree at another commit. Advisory tester
		// evidence is narrower: carry it only for the same head and base.
		if art.Metadata["head_sha"] != head || art.Metadata["base_sha"] != opts.BaseSHA || head == "" || opts.BaseSHA == "" {
			delete(art.Metadata, AcceptanceEvidenceKey)
		}
		art.Metadata["validation_source_time"] = source
		art.Metadata["validation_reused"] = "true"
		if art.Metadata["validation_source_digest"] == "" {
			art.Metadata["validation_source_digest"] = art.Metadata["content_sha256"]
		}
		age := time.Since(when).Round(time.Second)
		if age < 0 {
			return nil, nil
		}
		tree := current["validation_tree"]
		if len(tree) < 12 {
			return nil, nil
		}
		message := fmt.Sprintf("Reusing validation from %s (%s ago, tree %s; source head %s, submitted head %s)", source, age, tree[:12], report.HeadSHA, head)
		art.Summary = message
		// The original report body is stored in validation_report; repeated reuse
		// therefore retains evidence without accumulating reuse notices.
		elapsed := time.Since(started)
		art.Metadata["validation_reuse_duration"] = elapsed.String()
		art.Body = RestoreAcceptanceEvidence(message+"\nCurrent reuse wall time: "+DurationText(&elapsed)+"\n\n"+report.Artifact.Body, art.Metadata)
		digest := sha256.Sum256([]byte(art.Body))
		art.Metadata["content_sha256"] = hex.EncodeToString(digest[:])
		art.CreatedAt = time.Now().UTC()
		report.SourceHead = report.HeadSHA
		report.SourceTime = source
		reuseDuration := elapsed
		report.ReuseDuration = &reuseDuration
		art.Metadata["validation_reuse_duration"] = reuseDuration.String()
		report.HeadSHA = head
		art.Metadata["head_sha"] = head
		report.Artifact = art
		report.Reused = true
		return &report, nil
	}
	return nil, nil
}
