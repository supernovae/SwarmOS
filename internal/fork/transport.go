package fork

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/lease"
	"github.com/supernovae/swarmos/internal/tracker"
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

func FormatRequestComment(req lease.ForkRequest) (string, error) {
	if req.Version == 0 {
		req.Version = 1
	}
	if req.Version != 1 {
		return "", fmt.Errorf("unsupported fork request version %d", req.Version)
	}
	if strings.TrimSpace(req.Operation) == "" {
		return "", fmt.Errorf("fork request operation is required")
	}
	if req.IssueNumber <= 0 {
		return "", fmt.Errorf("fork request issue_number must be positive")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal fork request: %w", err)
	}
	body := RequestMarker + "\n" + string(raw)
	if len(body) > MaxForkCommentBytes {
		return "", fmt.Errorf("fork request payload (%d bytes) exceeds maximum GitHub comment size (%d bytes)", len(body), MaxForkCommentBytes)
	}
	return body, nil
}

func IsRequestComment(body string) bool {
	trimmed := strings.TrimSpace(body)
	return strings.HasPrefix(trimmed, RequestMarker)
}

func ParseRequestComment(body string) (*lease.ForkRequest, error) {
	if len(body) > MaxForkCommentBytes {
		return nil, fmt.Errorf("fork request exceeds maximum size")
	}
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, RequestMarker) {
		return nil, fmt.Errorf("comment does not start with %s", RequestMarker)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, RequestMarker))
	if payload == "" {
		return nil, fmt.Errorf("empty fork request payload")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
	dec.DisallowUnknownFields()
	var req lease.ForkRequest
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
	raw, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("marshal fork result: %w", err)
	}
	return ResultMarker + "\n" + string(raw), nil
}

func ParseResultComment(body string) (*ForkResult, error) {
	if len(body) > MaxForkCommentBytes {
		return nil, fmt.Errorf("fork result exceeds maximum size")
	}
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, ResultMarker) {
		return nil, fmt.Errorf("comment does not start with %s", ResultMarker)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, ResultMarker))
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

func IsTrustedBotComment(c *tracker.Comment) bool {
	if c == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.Author), ActionsBotLogin) {
		return false
	}
	if c.AuthorType != "" && !strings.EqualFold(strings.TrimSpace(c.AuthorType), "Bot") {
		return false
	}
	return true
}

type Gateway struct {
	Tracker      tracker.Tracker
	Store        coordination.Store
	ForkRepo     string
	PollInterval time.Duration
	PollTimeout  time.Duration
	Dispatcher   func(ctx context.Context, issueNumber int, comment *tracker.Comment) error
}

func (g *Gateway) Apply(ctx context.Context, req lease.ForkRequest) (*lease.IssueState, error) {
	if g == nil || g.Tracker == nil || g.Store == nil {
		return nil, fmt.Errorf("fork worker gateway is not configured")
	}
	timeout := g.PollTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req.Version = 1
	switch {
	case req.Operation == "acquire":
		if strings.TrimSpace(req.ForkRepo) == "" {
			req.ForkRepo = strings.TrimSpace(g.ForkRepo)
		}
		if req.ForkRepo == "" {
			return nil, fmt.Errorf("fork repository is required for acquire")
		}
	case req.Operation == "acquire-pr":
		// The PR's head repository, recorded at submission, identifies the fork.
		if req.PRNumber <= 0 {
			return nil, fmt.Errorf("PR number is required for acquire-pr")
		}
	case strings.TrimSpace(req.ClaimID) == "":
		return nil, lease.ErrClaimFence
	}

	body, err := FormatRequestComment(req)
	if err != nil {
		return nil, err
	}
	comment, err := g.Tracker.CreateComment(ctx, req.IssueNumber, body)
	if err != nil {
		return nil, fmt.Errorf("post fork request comment: %w", err)
	}
	if comment == nil || comment.ID <= 0 {
		return nil, fmt.Errorf("github tracker returned invalid comment ID for fork request")
	}
	reqID := comment.ID

	if g.Dispatcher != nil {
		_ = g.Dispatcher(ctx, req.IssueNumber, comment)
	}

	interval := g.PollInterval
	if interval <= 0 {
		interval = 1 * time.Second
	}
	since := comment.CreatedAt
	if since.IsZero() {
		since = time.Now()
	}
	// Anchor at the request (with a one-second overlap) so delayed result
	// visibility cannot be skipped when newer comments appear first.
	since = since.Add(-time.Second)
	for {
		st, done, checkErr := g.checkRequestOutcome(ctx, req, reqID, &since)
		if checkErr != nil {
			return nil, checkErr
		}
		if done {
			return st, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("no response from the upstream fork coordinator to request %d (%s) within %s; check the upstream repository's SwarmOS Fork Coordinator Actions run for that request: %w", reqID, req.Operation, timeout, ctx.Err())
		case <-timer.C:
		}
		if interval < 5*time.Second {
			interval *= 2
			if interval > 5*time.Second {
				interval = 5 * time.Second
			}
		}
	}
}

func (g *Gateway) checkRequestOutcome(ctx context.Context, req lease.ForkRequest, reqID int64, since *time.Time) (*lease.IssueState, bool, error) {
	var comments []*tracker.Comment
	var err error
	if incremental, ok := g.Tracker.(interface {
		ListCommentsSince(context.Context, int, time.Time) ([]*tracker.Comment, error)
	}); ok {
		comments, err = incremental.ListCommentsSince(ctx, req.IssueNumber, *since)
	} else {
		comments, err = g.Tracker.ListComments(ctx, req.IssueNumber)
	}
	if err != nil {
		return nil, false, fmt.Errorf("poll fork coordinator comments: %w", err)
	}
	if len(comments) > 1000 {
		return nil, false, fmt.Errorf("fork comment scan exceeds 1000 records")
	}
	bytes := 0
	for _, c := range comments {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if c == nil {
			continue
		}
		bytes += len(c.Body)
		if bytes > 8<<20 {
			return nil, false, fmt.Errorf("fork comment scan exceeds byte budget")
		}
		if c.ID <= reqID {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(c.Body), ResultMarker) {
			continue
		}
		res, parseErr := ParseResultComment(c.Body)
		if parseErr != nil || res.RequestID != reqID || res.IssueNumber != req.IssueNumber || res.Operation != req.Operation {
			continue
		}
		if !IsTrustedBotComment(c) || (!c.UpdatedAt.IsZero() && !c.CreatedAt.Equal(c.UpdatedAt)) {
			continue
		}
		if !res.Success {
			return nil, false, mapCoordinatorError(req.Operation, res.Error)
		}
		st, err := g.loadUpstreamState(ctx, req.IssueNumber)
		if err != nil {
			return nil, false, fmt.Errorf("load upstream state after fork result %d: %w", reqID, err)
		}
		if st.LastForkRequestID > reqID {
			return nil, false, fmt.Errorf("upstream state advanced past fork request %d (last=%d)", reqID, st.LastForkRequestID)
		}
		if st.LastForkRequestID != reqID || !expectedResultPresent(st, req) {
			return nil, false, fmt.Errorf("upstream state did not confirm fork request %d (%s)", reqID, req.Operation)
		}
		return st, true, nil
	}
	// A successfully committed operation can outlive a failed result-comment post.
	st, loadErr := g.loadUpstreamState(ctx, req.IssueNumber)
	if loadErr == nil && st != nil {
		if st.LastForkRequestID == reqID && expectedResultPresent(st, req) {
			return st, true, nil
		}
		if st.LastForkRequestID > reqID {
			return nil, false, fmt.Errorf("fork request %d was superseded", reqID)
		}
	}
	return nil, false, nil
}

func (g *Gateway) loadUpstreamState(ctx context.Context, issueNumber int) (*lease.IssueState, error) {
	raw, _, err := g.Store.Load(ctx, issueNumber)
	if err != nil {
		return nil, err
	}
	var st lease.IssueState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("decode upstream state: %w", err)
	}
	return &st, nil
}

func expectedResultPresent(st *lease.IssueState, req lease.ForkRequest) bool {
	if st == nil {
		return false
	}
	switch req.Operation {
	case "acquire":
		return st.Lease.Status == "active" && st.Lease.ClaimID != "" &&
			(req.ForkRepo == "" || strings.EqualFold(st.ForkRepo, req.ForkRepo))
	case "renew":
		return st.Lease.Status == "active" && st.Lease.ClaimID == req.ClaimID && st.Lease.Renewals > 0
	case "release":
		return st.Lease.Status == "released"
	case "handoff":
		return st.Lease.Status == "handoff" && st.Lease.Phase == req.Phase && st.Lease.NextPhase == req.NextPhase
	case "require-gate":
		return st.EffectiveGate != ""
	case "update-phase":
		if req.Phase != "" && st.Lease.Phase != req.Phase {
			return false
		}
		if req.Status != "" && st.Lease.Status != req.Status {
			return false
		}
		return true
	case "update-artifact":
		if req.Artifact == nil {
			return false
		}
		for _, a := range st.Artifacts {
			if a.Type == req.Artifact.Type {
				return true
			}
		}
		return false
	case "record-checkpoint":
		return st.Branch == req.Branch && st.HeadSHA == req.HeadSHA
	case "record-submission":
		return st.Lease.Status == "completed" && st.Lease.Phase == "submitted" &&
			st.PRNumber == req.PRNumber && strings.TrimSpace(st.PRURL) != ""
	case "acquire-pr":
		return st.Lease.Status == "active" && st.Lease.ClaimID != "" && st.PRNumber == req.PRNumber
	case "begin-pr-revision":
		return st.PRRevision != nil && st.PRRevision.Number == req.PRNumber &&
			st.PRRevision.RemoteHeadSHA == req.ExpectedHeadSHA && st.PRRevision.LocalHeadSHA == req.HeadSHA
	case "record-pr-head":
		return st.PRRevision != nil && st.PRRevision.Number == req.PRNumber && st.PRRevision.LocalHeadSHA == req.HeadSHA
	case "invalidate-pr-evidence":
		return st.PRRevision != nil && st.PRRevision.Number == req.PRNumber
	case "complete-pr-revision":
		return st.PRRevision == nil && st.PRNumber == req.PRNumber && st.HeadSHA == req.HeadSHA && st.Lease.Status == "completed"
	default:
		return false
	}
}

func mapCoordinatorError(op, msg string) error {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, lease.ErrClaimFence.Error()):
		return fmt.Errorf("%w: %s", lease.ErrClaimFence, msg)
	case strings.Contains(lower, lease.ErrApprovalRequired.Error()):
		return fmt.Errorf("%w: %s", lease.ErrApprovalRequired, msg)
	case strings.Contains(lower, lease.ErrAlreadyClaimed.Error()):
		return fmt.Errorf("%w: %s", lease.ErrAlreadyClaimed, msg)
	case strings.Contains(lower, lease.ErrClaimRaceLost.Error()):
		return fmt.Errorf("%w: %s", lease.ErrClaimRaceLost, msg)
	case strings.Contains(lower, lease.ErrMaxRenewals.Error()):
		return fmt.Errorf("%w: %s", lease.ErrMaxRenewals, msg)
	case strings.Contains(lower, coordination.ErrForkClaimLimit.Error()):
		return fmt.Errorf("%w: %s", coordination.ErrForkClaimLimit, msg)
	case strings.Contains(lower, coordination.ErrReplay.Error()):
		return fmt.Errorf("%w: %s", coordination.ErrReplay, msg)
	default:
		return fmt.Errorf("fork coordinator rejected %s: %s", op, msg)
	}
}

var _ lease.WorkerGateway = (*Gateway)(nil)
