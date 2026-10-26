package fork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
)

// releasedForkRequest is the request field set released coordinators decode
// strictly. It is copied from the v0.6.1 module source (v0.7.0 parses the same
// way); fields added since are omitempty and unset in these tests, so a
// smaller set only makes the check stricter.
type releasedForkRequest struct {
	Version         int                `json:"version"`
	Operation       string             `json:"operation"`
	IssueNumber     int                `json:"issue_number"`
	ForkRepo        string             `json:"fork_repo,omitempty"`
	ClaimID         string             `json:"claim_id,omitempty"`
	Agent           string             `json:"agent,omitempty"`
	Phase           string             `json:"phase,omitempty"`
	Status          string             `json:"status,omitempty"`
	Duration        string             `json:"duration,omitempty"`
	Gate            contract.GateMode  `json:"gate,omitempty"`
	Artifact        *contract.Artifact `json:"artifact,omitempty"`
	Branch          string             `json:"branch,omitempty"`
	HeadSHA         string             `json:"head_sha,omitempty"`
	ExpectedHeadSHA string             `json:"expected_head_sha,omitempty"`
	PRNumber        int                `json:"pr_number,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	NextPhase       string             `json:"next_phase,omitempty"`
}

var releasedCollapsedPayload = regexp.MustCompile("(?s)^\n[^\n]*\n\n<details><summary>[^<\n]*</summary>\n\n```json\n(.*)\n```\n\n</details>\\s*$")

// releasedCommentPayload is a copy of the released commentPayload.
func releasedCommentPayload(body, marker string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimLeft(body, " \t\r\n"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, marker) {
		return "", fmt.Errorf("comment does not start with %s", marker)
	}
	rest := strings.TrimPrefix(normalized, marker)
	if m := releasedCollapsedPayload.FindStringSubmatch(rest); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	return strings.TrimSpace(rest), nil
}

// releasedParseRequestComment is a copy of the released ParseRequestComment.
func releasedParseRequestComment(body string) (*releasedForkRequest, error) {
	if len(body) > MaxForkCommentBytes {
		return nil, fmt.Errorf("fork request exceeds maximum size")
	}
	payload, err := releasedCommentPayload(body, RequestMarker)
	if err != nil {
		return nil, err
	}
	if payload == "" {
		return nil, fmt.Errorf("empty fork request payload")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.DisallowUnknownFields()
	var req releasedForkRequest
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid fork request JSON: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data in fork request JSON")
	}
	if req.Version != 1 {
		return nil, fmt.Errorf("unsupported fork request version %d", req.Version)
	}
	if strings.TrimSpace(req.Operation) == "" || req.IssueNumber <= 0 {
		return nil, fmt.Errorf("fork request requires operation and positive issue_number")
	}
	return &req, nil
}

func TestReleasedCoordinatorAcceptsRequestComment(t *testing.T) {
	// The comment Apply actually posts, nonce included.
	tr := &commentTracker{}
	g := &Gateway{Tracker: tr, Store: releasedAt(100)}
	if _, err := g.Apply(t.Context(), releaseRequest()); err != nil {
		t.Fatal(err)
	}
	posted := tr.bodies[0]
	requestNonce(t, posted)
	got, err := releasedParseRequestComment(posted)
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
		old, err := releasedParseRequestComment(b)
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
		if _, err := releasedParseRequestComment(b); err == nil {
			t.Fatalf("%s: released copy unexpectedly accepted it", name)
		}
	}
}
