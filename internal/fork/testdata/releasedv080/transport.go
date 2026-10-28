package releasedv080

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	RequestMarker       = "<!-- swarm:fork-request:v1 -->"
	ResultMarker        = "<!-- swarm:fork-result:v1 -->"
	MaxForkCommentBytes = 60000
	ActionsBotLogin     = "github-actions[bot]"
)

type ForkResult struct {
	Version     int    `json:"version"`
	RequestID   int64  `json:"request_id"`
	IssueNumber int    `json:"issue_number"`
	Operation   string `json:"operation"`
	Success     bool   `json:"success"`
	Revision    string `json:"revision,omitempty"`
	ClaimID     string `json:"claim_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

func ParseRequestComment(body string) (*ForkRequest, error) {
	if len(body) > MaxForkCommentBytes {
		return nil, fmt.Errorf("fork request exceeds maximum size")
	}
	payload, err := commentPayload(body, RequestMarker)
	if err != nil {
		return nil, err
	}
	if payload == "" {
		return nil, fmt.Errorf("empty fork request payload")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.DisallowUnknownFields()
	var req ForkRequest
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

func FormatResultComment(res ForkResult) (string, error) {
	if res.Version == 0 {
		res.Version = 1
	}
	raw, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal fork result: %w", err)
	}
	return collapsedComment(ResultMarker, resultSummary(res), "Coordinator response", raw), nil
}

func ParseResultComment(body string) (*ForkResult, error) {
	if len(body) > MaxForkCommentBytes {
		return nil, fmt.Errorf("fork result exceeds maximum size")
	}
	payload, err := commentPayload(body, ResultMarker)
	if err != nil {
		return nil, err
	}
	if payload == "" {
		return nil, fmt.Errorf("empty fork result payload")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.DisallowUnknownFields()
	var res ForkResult
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("invalid fork result JSON: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data in fork result JSON")
	}
	if res.Version != 1 || res.RequestID <= 0 {
		return nil, fmt.Errorf("invalid fork result metadata")
	}
	return &res, nil
}

func collapsedComment(marker, summary, label string, payload []byte) string {
	return marker + "\n" + summary + "\n\n<details><summary>" + label + "</summary>\n\n```json\n" + string(payload) + "\n```\n\n</details>\n"
}

func commentPayload(body, marker string) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimLeft(body, " \t\r\n"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, marker) {
		return "", fmt.Errorf("comment does not start with %s", marker)
	}
	rest := strings.TrimPrefix(normalized, marker)
	if m := collapsedPayload.FindStringSubmatch(rest); m != nil {
		return strings.TrimSpace(m[1]), nil
	}
	return strings.TrimSpace(rest), nil
}

func resultSummary(res ForkResult) string {
	if !operationName.MatchString(res.Operation) {
		res.Operation = ""
	}
	if res.Success && res.Operation != "" {
		return fmt.Sprintf("✅ **SwarmOS coordinator:** `%s` succeeded", res.Operation)
	}
	if res.Operation == "" {
		return "❌ **SwarmOS coordinator:** request rejected (details below)"
	}
	return fmt.Sprintf("❌ **SwarmOS coordinator:** `%s` rejected (details below)", res.Operation)
}

var collapsedPayload = regexp.MustCompile("(?s)^\n[^\n]*\n\n<details><summary>[^<\n]*</summary>\n\n```json\n(.*)\n```\n\n</details>\\s*$")
var operationName = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)
