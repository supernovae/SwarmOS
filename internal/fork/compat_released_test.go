package fork

import (
	"bytes"
	"encoding/json"
	"github.com/supernovae/swarmos/internal/contract"
	released "github.com/supernovae/swarmos/internal/fork/testdata/releasedv080"
	"github.com/supernovae/swarmos/internal/lease"
	"reflect"
	"strings"
	"testing"
)

func TestReleasedCoordinatorAcceptsRequestComment(t *testing.T) {
	// The comment Apply actually posts, nonce included.
	tr := &commentTracker{}
	g := &Gateway{Tracker: tr, Store: releasedAt(100)}
	if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
		t.Fatal(err)
	}
	posted := tr.bodies[0]
	requestNonce(t, posted)
	got, err := released.ParseRequestComment(posted)
	if err != nil {
		t.Fatalf("released coordinator rejects posted request: %v\n%s", err, posted)
	}
	if got.Operation != "release" || got.IssueNumber != 1 || got.ClaimID != "c1" {
		t.Fatalf("released coordinator read %+v", got)
	}

	full := lease.ForkRequest{
		Version: 1, Operation: "record-submission", IssueNumber: 7, ForkRepo: "me/fork", ClaimID: "c1",
		Agent: "claude", Phase: "implement", Status: "active", Duration: "1h", Gate: contract.GateMode("human"),
		Artifact: &contract.Artifact{Type: "design"}, Branch: "swarm/7", HeadSHA: "0123456789abcdef",
		ExpectedHeadSHA: "fedcba9876543210", PRNumber: 9, Reason: "r", NextPhase: "validate",
	}
	body, err := formatRequestComment(full, "N0NCEN0NCEN0NCEN0NCE")
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{"lf": body, "crlf": strings.ReplaceAll(body, "\n", "\r\n")} {
		old, err := released.ParseRequestComment(b)
		if err != nil {
			t.Fatalf("%s: released coordinator rejects request: %v", name, err)
		}
		cur, err := ParseRequestComment(b)
		if err != nil {
			t.Fatalf("%s: current coordinator rejects request: %v", name, err)
		}
		oldJSON, _ := json.Marshal(old)
		curJSON, _ := json.Marshal(cur)
		if !bytes.Equal(oldJSON, curJSON) {
			t.Fatalf("%s: released and current coordinators read different requests:\n%s\n%s", name, oldJSON, curJSON)
		}
	}

	// The copy is faithful: a nonce inside the payload, as first attempted,
	// or on a line of its own breaks released coordinators.
	inPayload := RequestMarker + "\n" + `{"version":1,"operation":"release","issue_number":1,"request_nonce":"x"}`
	ownLine := strings.Replace(body, "\n", "\n<!-- swarm:request-nonce:N0NCEN0NCEN0NCEN0NCE -->\n", 1)
	for name, b := range map[string]string{"payload field": inPayload, "own line": ownLine} {
		if _, err := released.ParseRequestComment(b); err == nil {
			t.Fatalf("%s: released copy unexpectedly accepted it", name)
		}
	}
}

// Explicit cases are independent of Operations: extending the protocol requires
// choosing representative parameters here as well as updating the frozen oracle.
func releasedRequestCases() map[string]lease.ForkRequest {
	return map[string]lease.ForkRequest{
		"acquire":                {ForkRepo: "contributor/repo", Agent: "codex", Duration: "1h"},
		"renew":                  {ClaimID: "claim", Duration: "1h"},
		"release":                {ClaimID: "claim", Reason: "done"},
		"handoff":                {ClaimID: "claim", Phase: "implement", NextPhase: "validate"},
		"require-gate":           {ClaimID: "claim", Gate: "human", Reason: "review"},
		"update-phase":           {ClaimID: "claim", Phase: "implement", Status: "active"},
		"update-artifact":        {ClaimID: "claim", Artifact: &contract.Artifact{Type: "implementation", Body: "evidence"}},
		"record-checkpoint":      {ClaimID: "claim", Branch: "swarm/7", HeadSHA: "abcdef0123456789"},
		"record-submission":      {ClaimID: "claim", PRNumber: 9, Branch: "swarm/7", HeadSHA: "abcdef0123456789"},
		"acquire-pr":             {PRNumber: 9, Agent: "codex", Duration: "1h"},
		"begin-pr-revision":      {ClaimID: "claim", PRNumber: 9, ExpectedHeadSHA: "abcdef0123456789", HeadSHA: "123456789abcdef", RevisionIntent: "rebase", RebaseContext: &lease.RebaseContext{Status: "clean"}},
		"record-pr-head":         {ClaimID: "claim", PRNumber: 9, HeadSHA: "123456789abcdef"},
		"invalidate-pr-evidence": {ClaimID: "claim", PRNumber: 9, Reason: "changed"},
		"complete-pr-revision":   {ClaimID: "claim", PRNumber: 9, ExpectedHeadSHA: "abcdef0123456789", HeadSHA: "123456789abcdef"},
	}
}

func TestReleasedCompatibilityOperationCoverage(t *testing.T) {
	cases := releasedRequestCases()
	seen := map[string]bool{}
	for _, op := range Operations {
		if seen[op] {
			t.Errorf("duplicate operation %s", op)
		}
		seen[op] = true
		if _, ok := cases[op]; !ok {
			t.Errorf("missing compatibility case for %s", op)
		}
	}
	for op := range cases {
		if !seen[op] {
			t.Errorf("extra compatibility case %s", op)
		}
	}
	t.Logf("v0.8.0 (58f33b8101acecb2c479e548addae8760a7e8733): %d operations", len(cases))
}

// This literal deliberately lists every wire field, including nested payloads.
// Reflection below checks it but never fills it, so new omitempty fields fail.
const denseReleasedRequest = `{
 "version":1,"operation":"begin-pr-revision","issue_number":7,
 "fork_repo":"contributor/repo","claim_id":"claim","agent":"codex",
 "phase":"implement","status":"active","duration":"1h","gate":"human",
 "branch":"swarm/7","head_sha":"abcdef0123456789","expected_head_sha":"123456789abcdef",
 "pr_number":9,"reason":"revision","next_phase":"validate","revision_intent":"rebase",
 "artifact":{"version":1,"id":"artifact","issue_number":7,"type":"implementation","producer":"codex:developer","verdict":"PASS","summary":"summary","body":"body","created_at":"2026-10-28T00:00:00Z","metadata":{"key":"value"}},
 "rebase_context":{"repair_paths":["a.go"],"repair_attempt":1,"conflicted_paths":["a.go"],"status":"resolved",
 "plan":{"harness":"codex","harness_version":"1","model":"model","sandbox":"native","commit":"abc","paths":["a.go"],"body":"plan","digest":"digest","plan_digest":"plan","base_sha":"base","agent":"codex","guidance":["repair"]},
 "resolution":{"commit":"abc","paths":["a.go"],"agent":"codex","harness":"codex","harness_version":"1","model":"model","sandbox":"native","guidance":["repair"]}}
}`

func TestReleasedCompatibilityFieldCoverage(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(denseReleasedRequest), &payload); err != nil {
		t.Fatal(err)
	}
	var check func(reflect.Type, map[string]any, string)
	check = func(typ reflect.Type, fields map[string]any, path string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			value, ok := fields[name]
			if !ok || value == nil {
				t.Errorf("missing populated field %s%s", path, name)
				continue
			}
			nested := f.Type
			for nested.Kind() == reflect.Pointer {
				nested = nested.Elem()
			}
			if nested.Kind() == reflect.Struct {
				if obj, ok := value.(map[string]any); ok {
					check(nested, obj, path+name+".")
				}
			}
		}
	}
	check(reflect.TypeFor[lease.ForkRequest](), payload, "")
}

func assertReleasedWireEqual(t *testing.T, want, got any) {
	t.Helper()
	normalize := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var obj any
		if err = json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	if !reflect.DeepEqual(normalize(want), normalize(got)) {
		t.Fatalf("wire values differ:\nwant %+v\ngot %+v", want, got)
	}
}

func TestReleasedCoordinatorAcceptsAllOperations(t *testing.T) {
	cases := releasedRequestCases()
	var dense lease.ForkRequest
	if err := json.Unmarshal([]byte(denseReleasedRequest), &dense); err != nil {
		t.Fatal(err)
	}
	cases["dense"] = dense
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			req.Version = 1
			req.IssueNumber = 7
			if name != "dense" {
				req.Operation = name
			}
			for _, nonce := range []string{"", "N0NCEN0NCEN0NCEN0NCE"} {
				var body string
				var err error
				if nonce == "" {
					body, err = FormatRequestComment(req)
				} else {
					body, err = formatRequestComment(req, nonce)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, wire := range []string{body, strings.ReplaceAll(body, "\n", "\r\n")} {
					got, err := released.ParseRequestComment(wire)
					if err != nil {
						t.Fatalf("released parser: %v", err)
					}
					assertReleasedWireEqual(t, req, got)
				}
			}
		})
	}
}

func TestReleasedCoordinatorRejectsUnknownRequestField(t *testing.T) {
	body, err := FormatRequestComment(lease.ForkRequest{Operation: "release", IssueNumber: 7, ClaimID: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = released.ParseRequestComment(body); err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, "{", "{\n  \"request_nonce\": \"unknown\",", 1)
	if _, err = released.ParseRequestComment(body); err == nil || !strings.Contains(err.Error(), `unknown field "request_nonce"`) {
		t.Fatalf("expected unknown field rejection, got %v", err)
	}
	t.Log(err)
}

func TestCurrentWorkerParsesReleasedResults(t *testing.T) {
	for op := range releasedRequestCases() {
		t.Run(op, func(t *testing.T) {
			for _, res := range []released.ForkResult{
				{RequestID: 123, IssueNumber: 7, Operation: op, Success: true, Revision: "revision", ClaimID: "claim"},
				{RequestID: 123, IssueNumber: 7, Operation: op, Error: "claim fence"},
				{RequestID: 123, IssueNumber: 7, Operation: op, Success: true},
				{RequestID: 123, Error: "invalid request"},
			} {
				body, err := released.FormatResultComment(res)
				if err != nil {
					t.Fatal(err)
				}
				res.Version = 1
				for _, wire := range []string{body, strings.ReplaceAll(body, "\n", "\r\n")} {
					old, err := released.ParseResultComment(wire)
					if err != nil {
						t.Fatal(err)
					}
					assertReleasedWireEqual(t, res, old)
					got, err := ParseResultComment(wire)
					if err != nil {
						t.Fatalf("current parser rejects v0.8.0 result: %v", err)
					}
					assertReleasedWireEqual(t, res, got)
				}
			}
		})
	}
}
