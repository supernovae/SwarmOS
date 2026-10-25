package fork

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	"gopkg.in/yaml.v3"
)

func TestForkCommentsCollapseJSONBehindAReadableSummary(t *testing.T) {
	req := lease.ForkRequest{Version: 1, Operation: "record-checkpoint", IssueNumber: 7, ClaimID: "c1", Branch: "swarm/issue-7-c1", HeadSHA: strings.Repeat("a", 40),
		Artifact: &contract.Artifact{Body: "```go\nfmt.Println(\"fenced markdown inside the payload\")\n```\n"}}
	body, err := FormatRequestComment(req)
	if err != nil {
		t.Fatal(err)
	}
	firstLines := strings.SplitN(body, "\n", 3)
	if firstLines[0] != RequestMarker || !strings.Contains(firstLines[1], "`record-checkpoint` for issue #7 at `aaaaaaa`") || !strings.Contains(body, "<details><summary>Request details</summary>") {
		t.Fatalf("request comment is not collapsed behind a summary:\n%s", body)
	}
	parsed, err := ParseRequestComment(body)
	if err != nil || parsed.Operation != req.Operation || parsed.HeadSHA != req.HeadSHA || parsed.Artifact.Body != req.Artifact.Body {
		t.Fatalf("round trip = %+v, %v", parsed, err)
	}
	if _, err := ParseRequestComment(strings.ReplaceAll(body, "\n", "\r\n")); err != nil {
		t.Fatalf("CRLF-normalized comment rejected: %v", err)
	}
	legacy := RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":7,"claim_id":"c1"}`
	if parsed, err := ParseRequestComment(legacy); err != nil || parsed.Operation != "renew" {
		t.Fatalf("original raw-JSON layout = %+v, %v", parsed, err)
	}

	for _, res := range []ForkResult{{Version: 1, RequestID: 9, IssueNumber: 7, Operation: "renew", Success: true}, {Version: 1, RequestID: 9, IssueNumber: 7, Operation: "renew", Error: "claim fence"}} {
		out, err := FormatResultComment(res)
		if err != nil {
			t.Fatal(err)
		}
		want := "✅ **SwarmOS coordinator:** `renew` succeeded"
		if !res.Success {
			want = "❌ **SwarmOS coordinator:** `renew` rejected (details below)"
		}
		if strings.SplitN(out, "\n", 3)[1] != want {
			t.Fatalf("result summary:\n%s", out)
		}
		if got, err := ParseResultComment(out); err != nil || got.Error != res.Error || got.Success != res.Success {
			t.Fatalf("result round trip = %+v, %v", got, err)
		}
	}
	forged, _ := FormatResultComment(ForkResult{Version: 1, RequestID: 9, Operation: "renew` [click](https://evil)"})
	if strings.Contains(strings.SplitN(forged, "\n", 3)[1], "evil") {
		t.Fatal("result summary echoed an untrusted operation name")
	}
}

// The workflow screens requests in Python before checkout; it must accept the
// generated layout and the original one, and reject trailing content.
func TestWorkflowAdmissionParsesForkComments(t *testing.T) {
	// The coordinator workflow runs on Ubuntu; Windows python3 may be a stub.
	if runtime.GOOS == "windows" {
		t.Skip("coordinator admission runs on Linux runners")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	raw, err := os.ReadFile("../harness/templates/github/workflows/swarm-fork-coordinator.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	script := ""
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Name, "Authenticate request") {
				script = step.Run
			}
		}
	}
	start := strings.Index(script, "import json")
	end := strings.Index(script, "def fetch(")
	if start < 0 || end < 0 {
		t.Fatal("admission script not found")
	}
	admission := script[start:end]

	request, _ := FormatRequestComment(lease.ForkRequest{Version: 1, Operation: "complete-pr-revision", IssueNumber: 3, ClaimID: "c1", PRNumber: 4, HeadSHA: strings.Repeat("b", 40)})
	legacy := RequestMarker + "\n" + `{"version":1,"operation":"renew","issue_number":3,"claim_id":"c1"}`
	for body, ok := range map[string]bool{request: true, legacy: true, request + "\ninjected": false, strings.Replace(request, `"operation": "complete-pr-revision"`, `"operation": "drop-tables"`, 1): false} {
		event := map[string]any{
			"issue":   map[string]any{"number": 3},
			"comment": map[string]any{"body": body, "user": map[string]any{"type": "User", "id": 1}, "created_at": "t", "updated_at": "t"},
		}
		data, _ := json.Marshal(event)
		path := filepath.Join(t.TempDir(), "event.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, "-c", admission)
		cmd.Env = append(os.Environ(), "GITHUB_EVENT_PATH="+path, "GITHUB_REPOSITORY=octo/swarm")
		out, err := cmd.CombinedOutput()
		if (err == nil) != ok {
			t.Fatalf("admission of %q: ok=%v want %v (%s)", body[:min(len(body), 80)], err == nil, ok, out)
		}
	}
}

func TestForkRequestRejectsCarryAuthority(t *testing.T) {
	for _, field := range []string{`"policy_tree_digest":"forged"`, `"carry_forward":true`, `"approval_transition":{}`, `"base_sha":"forged"`, `"rebase_context":{"status":"completed","patch_identity":"unchanged","verified":true}`, `"rebase_context":{"status":"completed","resolution":{"commit":"` + strings.Repeat("a", 40) + `","paths":["x.go"],"agent":"codex","reported":false}}`} {
		body := RequestMarker + "\n" + `{"version":1,"operation":"begin-pr-revision","issue_number":7,"revision_intent":"rebase",` + field + "}"
		if _, err := ParseRequestComment(body); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("authority field %s: expected unknown field rejection, got %v", field, err)
		}
	}
	// A resolution report without authority fields is accepted as a report.
	body := RequestMarker + "\n" + `{"version":1,"operation":"invalidate-pr-evidence","issue_number":7,"pr_number":9,"revision_intent":"rebase","rebase_context":{"status":"completed","resolution":{"commit":"` + strings.Repeat("a", 40) + `","paths":["x.go"],"agent":"codex"}}}`
	req, err := ParseRequestComment(body)
	if err != nil || req.RebaseContext == nil || req.RebaseContext.Resolution == nil || req.RebaseContext.Resolution.Agent != "codex" {
		t.Fatalf("resolution report rejected: %+v, %v", req, err)
	}
}
