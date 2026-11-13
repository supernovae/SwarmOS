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
	for _, kind := range []string{"match", "timing", "tree", "base", "policy", "claim", "version", "origin", "owner", "trusted-claim", "FAIL", "missing", "malformed", "dev", "dirty", "empty", "fork"} {
		t.Run(kind, func(t *testing.T) {
			art := original
			art.Metadata = map[string]string{}
			for k, v := range original.Metadata {
				art.Metadata[k] = v
			}
			o := opts
			switch kind {
			case "timing":
				var r ValidationReport
				if err := json.Unmarshal([]byte(art.Metadata["validation_report"]), &r); err != nil {
					t.Fatal(err)
				}
				r.Timings = nil
				raw, _ := json.Marshal(r)
				art.Metadata["validation_report"] = string(raw)
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
			if (reused != nil) != (kind == "match" || kind == "timing") {
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

func TestIssue470CrossClaimReuseBindings(t *testing.T) {
	root := setupGitRepoForValidation(t)
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte(".swarm/active/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := ValidateOptions{BaseSHA: gitRevision(t, root, "HEAD"), Version: "v1.2.3", ClaimID: "A"}
	report, err := Validate(t.Context(), root, &contract.WorkUnit{IssueNumber: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	original := report.Artifact
	original.Metadata["validation_trusted_origin"] = "coordinator"
	original.Metadata["validation_trusted_owner"] = "owner"
	original.Metadata["validation_trusted_claim"] = "A"
	StoreAcceptanceSnapshot(original.Metadata, CaptureAcceptanceEvidence(evidenceBlock(`{"version":1,"criteria":[{"index":1,"criterion":"works","status":"met","evidence":"observed"}]}`)))
	now := original.CreatedAt.Add(time.Minute)
	for _, kind := range []string{"match", "head", "owner", "tree", "base", "policy", "version", "FAIL", "untrusted", "incoherent", "missing", "future", "malformed", "expired", "default", "fork", "ci", "rebound-inside", "rebound-exact", "rebound-outside", "newer-fail", "newer-untrusted", "source-empty"} {
		t.Run(kind, func(t *testing.T) {
			art := original
			art.Metadata = map[string]string{}
			for k, v := range original.Metadata {
				art.Metadata[k] = v
			}
			o := opts
			o.ClaimID = "B"
			o.DirectIssueReuse = true
			switch kind {
			case "head":
				art.Metadata["head_sha"] = "other"
			case "source-empty":
				art.Metadata["validation_source_claim"] = ""
			case "owner":
				art.Metadata["validation_trusted_owner"] = "other"
			case "tree", "base", "policy", "version":
				art.Metadata["validation_"+kind] = "other"
			case "FAIL":
				art.Verdict = "FAIL"
			case "untrusted":
				art.Metadata["validation_trusted_origin"] = "worker"
			case "incoherent":
				art.Metadata["validation_claim"] = "B"
			case "missing":
				delete(art.Metadata, "validation_claim")
			case "future":
				art.Metadata["validation_source_time"] = now.Add(time.Nanosecond).Format(time.RFC3339Nano)
			case "malformed":
				art.Metadata["validation_source_time"] = "bad"
			case "expired":
				art.Metadata["validation_source_time"] = now.Add(-2*time.Hour - time.Nanosecond).Format(time.RFC3339Nano)
			case "default":
				o.DirectIssueReuse = false
			case "fork":
				o.ForkMode = true
			case "ci":
				o.CIMode = true
			case "rebound-inside", "rebound-exact", "rebound-outside":
				art.Metadata["validation_source_claim"] = "A"
				art.Metadata["validation_claim"] = "B"
				art.Metadata["validation_trusted_claim"] = "B"
				age := 2 * time.Hour
				if kind == "rebound-inside" {
					age -= time.Nanosecond
				}
				if kind == "rebound-outside" {
					age += time.Nanosecond
				}
				art.Metadata["validation_source_time"] = now.Add(-age).Format(time.RFC3339Nano)
			}
			artifacts := []contract.Artifact{art}
			if kind == "newer-fail" || kind == "newer-untrusted" {
				newer := art
				newer.Metadata = map[string]string{}
				for k, v := range art.Metadata {
					newer.Metadata[k] = v
				}
				if kind == "newer-fail" {
					newer.Verdict = "FAIL"
				} else {
					newer.Metadata["validation_trusted_origin"] = "worker"
				}
				artifacts = append(artifacts, newer)
			}
			reused, err := reuseAt(t.Context(), root, o, "owner", artifacts, now)
			want := kind == "head" || kind == "match" || kind == "rebound-inside" || kind == "rebound-exact"
			if err != nil || (reused != nil) != want {
				t.Fatalf("reuse=%v want=%v err=%v", reused != nil, want, err)
			}
			if reused != nil {
				if got := AcceptanceSnapshotFromMetadata(reused.Artifact.Metadata).State; (got == "valid") != (kind != "head") {
					t.Fatalf("acceptance state=%s", got)
				}
				if original.Metadata[AcceptanceEvidenceKey] == "" {
					t.Fatal("mutated input evidence")
				}
				if reused.Artifact.Metadata["validation_source_claim"] != "A" || reused.Artifact.Metadata["validation_claim"] != "B" {
					t.Fatal("lost provenance")
				}
				// Model the trusted writer's current authority stamp for another reuse.
				reused.Artifact.Metadata["validation_trusted_claim"] = "B"
				again, err := reuseAt(t.Context(), root, o, "owner", []contract.Artifact{reused.Artifact}, now)
				if err != nil || again == nil {
					t.Fatalf("repeat: %v", err)
				}
				if again.Artifact.Metadata["validation_source_time"] != reused.Artifact.Metadata["validation_source_time"] {
					t.Fatal("renewed source time")
				}

				next := o
				next.ClaimID = "C"
				third, err := reuseAt(t.Context(), root, next, "owner", []contract.Artifact{again.Artifact}, now)
				if err != nil || third == nil || third.Artifact.Metadata["validation_source_claim"] != "A" || third.Artifact.Metadata["validation_source_time"] != again.Artifact.Metadata["validation_source_time"] {
					t.Fatalf("B to C provenance: %v", err)
				}
				if kind != "head" && third.Artifact.Metadata[AcceptanceEvidenceKey] != original.Metadata[AcceptanceEvidenceKey] {
					t.Fatal("lost repeated evidence")
				}
				if _, exists := original.Metadata["validation_source_claim"]; exists {
					t.Fatal("mutated input provenance")
				}
				expired, err := reuseAt(t.Context(), root, o, "owner", []contract.Artifact{again.Artifact}, now.Add(3*time.Hour))
				if err != nil || expired != nil {
					t.Fatal("rebound evidence escaped budget")
				}
			}
		})
	}
}

func TestIssue470InvalidSourcePolicy(t *testing.T) {
	for _, policy := range []string{"invalid", "version: 1\nlease:\n  duration: 1h\n  max_renewals: -1\n", "version: 1\nlease:\n  duration: 1000000h\n  max_renewals: 1000000\n"} {
		t.Run(policy, func(t *testing.T) {
			root := setupGitRepoForValidation(t)
			if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte(".swarm/active/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".swarm/config.yaml"), []byte(policy), 0644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "invalid policy"}} {
				if _, err := gitOutput(root, args...); err != nil {
					t.Fatal(err)
				}
			}
			opts := ValidateOptions{BaseSHA: gitRevision(t, root, "HEAD"), Version: "v1.2.3", ClaimID: "B", DirectIssueReuse: true}
			md, err := fingerprint(t.Context(), root, opts)
			if err != nil {
				t.Fatal(err)
			}
			md["validation_trusted_origin"] = "coordinator"
			md["validation_trusted_owner"] = "owner"
			md["validation_trusted_claim"] = "B"
			md["validation_source_claim"] = "A"
			md["validation_source_time"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
			md["validation_report"] = `{"passed":true}`
			art := contract.Artifact{Type: contract.ArtifactValidation, Verdict: "PASS", CreatedAt: time.Now(), Metadata: md}
			reused, err := Reuse(t.Context(), root, opts, "owner", []contract.Artifact{art})
			if err != nil || reused != nil {
				t.Fatalf("invalid policy reused: %v", err)
			}
		})
	}
}
