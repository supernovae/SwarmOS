package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestFingerprintReuseBindings(t *testing.T) {
	t.Parallel()
	root := setupGitRepoForValidation(t)
	// Active artifacts are coordinator scratch, never checkpoint input.
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte(".swarm/active/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := ValidateOptions{BaseSHA: gitRevision(t, root, "HEAD"), Version: "v1.2.3", ClaimID: "claim"}
	report, err := Validate(context.Background(), root, &contract.WorkUnit{IssueNumber: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Artifact.Metadata["validation_tree"] == "" {
		t.Fatal("missing fingerprint")
	}
	original := report.Artifact
	// Unit fixtures model the fenced writer; integration tests exercise that API.
	original.Metadata["validation_trusted_origin"] = "coordinator"
	original.Metadata["validation_trusted_owner"] = "owner"
	original.Metadata["validation_trusted_claim"] = "claim"
	for _, kind := range []string{"match", "tree", "base", "policy", "claim", "version", "origin", "owner", "trusted-claim", "FAIL", "missing", "malformed", "dev", "dirty", "empty", "fork"} {
		t.Run(kind, func(t *testing.T) {
			art := original
			art.Metadata = map[string]string{}
			for k, v := range original.Metadata {
				art.Metadata[k] = v
			}
			o := opts
			switch kind {
			case "tree", "base", "policy", "claim", "version":
				art.Metadata["validation_"+kind] = "other"
			case "origin", "owner":
				art.Metadata["validation_trusted_"+kind] = "other"
			case "trusted-claim":
				art.Metadata["validation_trusted_claim"] = "other"
			case "FAIL":
				art.Verdict = "FAIL"
			case "missing":
				delete(art.Metadata, "validation_tree")
			case "malformed":
				art.Metadata["validation_report"] = "bad"
			case "dev":
				o.Version = "dev"
				art.Metadata["validation_version"] = o.Version
			case "dirty":
				o.Version = "v1.2.3+dirty"
				art.Metadata["validation_version"] = o.Version
			case "empty":
				o.Version = ""
				art.Metadata["validation_version"] = o.Version
			case "fork":
				o.ForkMode = true
			}
			reused, err := Reuse(context.Background(), root, o, "owner", []contract.Artifact{art})
			if err != nil {
				t.Fatal(err)
			}
			if (reused != nil) != (kind == "match") {
				t.Fatalf("reuse=%v", reused != nil)
			}
			if reused != nil {
				if !reused.Reused {
					t.Fatal("missing reuse disclosure")
				}
				again, err := Reuse(context.Background(), root, o, "owner", []contract.Artifact{reused.Artifact})
				if err != nil || again == nil {
					t.Fatalf("repeat reuse: %v", err)
				}
				if again.Artifact.Metadata["validation_source_time"] != original.CreatedAt.Format(time.RFC3339Nano) {
					t.Fatal("source time reset")
				}
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if reused, err := Reuse(context.Background(), root, opts, "owner", []contract.Artifact{original}); err != nil || reused != nil {
		t.Fatalf("edited candidate reused: %v", err)
	}
}

func TestFingerprintContentChangingCheckIsIneligible(t *testing.T) {
	t.Parallel()
	root := setupGitRepoForValidation(t)
	policy := "required_checks:\n - name: change\n   command: printf changed > main.go\n   timeout: 1m\nprotected_paths: ['.github/**']\n"
	if err := os.WriteFile(filepath.Join(root, ".swarm/validation/required-checks.yaml"), []byte(policy), 0644); err != nil {
		t.Fatal(err)
	}
	// Commit a trusted policy with a check that mutates candidate content.
	for _, args := range [][]string{{"add", ".swarm/validation/required-checks.yaml"}, {"commit", "-m", "policy"}} {
		if _, err := gitOutput(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Validate(context.Background(), root, &contract.WorkUnit{IssueNumber: 1}, ValidateOptions{BaseSHA: gitRevision(t, root, "HEAD"), Version: "v1.2.3", ClaimID: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Artifact.Metadata["validation_tree"] != "" {
		t.Fatal("mutating check received reusable evidence")
	}
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) == 0 {
		t.Fatal(err)
	}
}

func TestFingerprintUsesTrustedPolicyBytes(t *testing.T) {
	t.Parallel()
	root := setupGitRepoForValidation(t)
	base := gitRevision(t, root, "HEAD")
	trusted, err := gitOutput(root, "show", base+":.swarm/validation/required-checks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".swarm/validation/required-checks.yaml"), []byte("changed candidate policy"), 0644); err != nil {
		t.Fatal(err)
	}
	opts := ValidateOptions{BaseSHA: base, Version: "dev", ClaimID: "claim"}
	report, err := Validate(context.Background(), root, &contract.WorkUnit{IssueNumber: 1, AllowHarnessChanges: true}, opts)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(trusted)
	m := report.Artifact.Metadata
	if m["validation_policy"] != hex.EncodeToString(sum[:]) || m["validation_base"] != base || m["validation_claim"] != "claim" || m["validation_version"] != "dev" || m["validation_tree"] == "" {
		t.Fatalf("bad fingerprint: %+v", m)
	}
}
