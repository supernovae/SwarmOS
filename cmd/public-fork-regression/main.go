// public-fork-regression is an opt-in operator tool, never a coordinator.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/lease"
	prservice "github.com/supernovae/swarmos/internal/pr"
	"github.com/supernovae/swarmos/internal/safefs"
	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/validation"
	"github.com/supernovae/swarmos/internal/workflow"
	"github.com/supernovae/swarmos/internal/workspace"
)

var errPaused = errors.New("paused: independent plan approval required")
var errResponse = errors.New("unexpected structured response")
var slugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var idRE = regexp.MustCompile(`^[a-f0-9]{32}$`)
var shaRE = regexp.MustCompile(`^[a-f0-9]{40}$`)

type options struct {
	Upstream, Fork, UpstreamRoot, ForkRoot, Gate, Resume string
	DryRun                                               bool
}
type subprocess func(context.Context, string, ...string) ([]byte, error)
type driver struct {
	o     options
	call  subprocess
	out   io.Writer
	actor string
}
type record struct {
	Options                         options
	Actor, ID, Stage, PublishedHead string
	Issue, PR                       int
}

func main() {
	err := run(context.Background(), os.Args[1:], func(ctx context.Context, name string, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, name, args...)
		return c.Output()
	}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errPaused) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
func parseOptions(args []string) (options, error) {
	var o options
	f := flag.NewFlagSet("public-fork-regression", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.Upstream, "upstream", "", "public upstream OWNER/REPO")
	f.StringVar(&o.Fork, "fork", "", "personal fork OWNER/REPO")
	f.StringVar(&o.UpstreamRoot, "upstream-root", "", "prepared upstream checkout")
	f.StringVar(&o.ForkRoot, "fork-root", "", "prepared fork checkout")
	f.StringVar(&o.Gate, "gate", "human", "agent or human")
	f.StringVar(&o.Resume, "resume", "", "retained pause record")
	f.BoolVar(&o.DryRun, "dry-run", false, "render without subprocesses or writes")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, flag.ErrHelp
		}
		return o, errors.New("invalid driver options (use --help)")
	}
	if f.NArg() != 0 || !validSlug(o.Upstream) || !validSlug(o.Fork) || strings.EqualFold(o.Upstream, o.Fork) || o.UpstreamRoot == "" || o.ForkRoot == "" || (o.Gate != "agent" && o.Gate != "human") {
		return o, errors.New("explicit distinct public repositories, checkout roots and agent|human gate required")
	}
	o.UpstreamRoot, _ = filepath.Abs(o.UpstreamRoot)
	o.ForkRoot, _ = filepath.Abs(o.ForkRoot)
	if o.UpstreamRoot == o.ForkRoot {
		return o, errors.New("distinct checkout roots required")
	}
	return o, nil
}
func validSlug(s string) bool {
	return slugRE.MatchString(s) && !strings.EqualFold(s, "supernovae/swarmos") && !strings.HasSuffix(s, ".")
}
func run(ctx context.Context, args []string, call subprocess, out io.Writer) error {
	o, err := parseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(out, "public-fork-regression --upstream OWNER/REPO --fork OWNER/REPO --upstream-root PATH --fork-root PATH [--gate human|agent] [--dry-run] [--resume PATH]")
		return nil
	}
	if err != nil {
		return err
	}
	d := driver{o: o, call: call, out: out}
	if o.DryRun {
		d.preview()
		return nil
	}
	if err = d.preflight(ctx); err != nil {
		return err
	}
	if o.Resume != "" {
		return d.resumeLifecycle(ctx)
	}
	return d.runLifecycle(ctx)
}
func (d *driver) command(ctx context.Context, step, name string, args ...string) ([]byte, error) {
	data, err := d.call(ctx, name, args...)
	if err != nil {
		fmt.Fprintln(d.out, step+": failed")
		return nil, fmt.Errorf("%s failed", step)
	}
	if len(data) > 8<<20 {
		return nil, errResponse
	}
	fmt.Fprintln(d.out, step+": exited 0")
	return data, nil
}
func (d *driver) swarm(ctx context.Context, contributor bool, step string, args ...string) ([]byte, error) {
	root, remote := d.o.UpstreamRoot, "origin"
	if contributor {
		root, remote = d.o.ForkRoot, "upstream"
	}
	base := []string{"--backend", "github", "--repo", d.o.Upstream, "--root", root, "--upstream-remote", remote}
	if contributor {
		base = append(base, "--fork", "--fork-remote", "origin")
	}
	return d.command(ctx, step, "swarm", append(append(base, args...), "--json")...)
}

// Check exact key spelling and presence before decoding: encoding/json alone
// accepts case-insensitive struct keys and silently fills missing booleans.
func exact(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	raw, err := jsonValue(dec)
	if err != nil {
		return errResponse
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errResponse
	}
	if !shape(raw, reflect.TypeOf(v).Elem()) {
		return errResponse
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return errResponse
	}
	return nil
}

// jsonValue refuses duplicate keys at every depth rather than accepting the
// last of two contradictory identities or validation outcomes.
func jsonValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return nil, e
			}
			key, ok := k.(string)
			if !ok {
				return nil, errResponse
			}
			if _, exists := m[key]; exists {
				return nil, errResponse
			}
			v, e := jsonValue(d)
			if e != nil {
				return nil, e
			}
			m[key] = v
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return nil, errResponse
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, e := jsonValue(d)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return nil, errResponse
		}
		return a, nil
	default:
		return nil, errResponse
	}
}
func shape(raw any, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		if raw == nil {
			return true
		}
		t = t.Elem()
	}
	if t == reflect.TypeFor[json.RawMessage]() || t.Implements(reflect.TypeFor[json.Unmarshaler]()) || reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return true
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		fields := map[string]reflect.Type{}
		required := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")
			key := tag[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			fields[key] = f.Type
			required[key] = len(tag) == 1
		}
		for k, v := range m {
			ft, ok := fields[k]
			if !ok || !shape(v, ft) {
				return false
			}
			delete(required, k)
		}
		for _, needed := range required {
			if needed {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if raw == nil {
			return true
		}
		a, ok := raw.([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			if !shape(v, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.Map:
		if raw == nil {
			return true
		}
		m, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		for _, v := range m {
			if !shape(v, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.Interface:
		return true
	default:
		return raw != nil
	}
}

type repository struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Fork     bool   `json:"fork"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Parent string `json:"parent"`
	Push   bool   `json:"push"`
}

func (d *driver) preflight(ctx context.Context) error {
	var user struct {
		Login string `json:"login"`
	}
	b, e := d.command(ctx, "identity", "gh", "api", "user", "--hostname", "github.com", "--jq", "{login: .login}")
	if e != nil {
		return e
	}
	if exact(b, &user) != nil || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`).MatchString(user.Login) {
		return errResponse
	}
	d.actor = user.Login
	repos := []repository{{}, {}}
	for i, slug := range []string{d.o.Upstream, d.o.Fork} {
		b, e = d.command(ctx, "public repository", "gh", "api", "repos/"+slug, "--hostname", "github.com", "--jq", "{full_name: .full_name, private: .private, fork: .fork, owner: {login: .owner.login, type: .owner.type}, parent: (.parent.full_name // \"\"), push: (.permissions.push // false)}")
		if e != nil {
			return e
		}
		if exact(b, &repos[i]) != nil {
			return errResponse
		}
		r := repos[i]
		if !validSlug(r.FullName) || !strings.EqualFold(r.FullName, slug) || r.Private || r.Owner.Login == "" || !strings.EqualFold(strings.Split(r.FullName, "/")[0], r.Owner.Login) {
			return errors.New("public repository identity refused")
		}
	}
	if !repos[0].Push || (repos[0].Owner.Type != "User" && repos[0].Owner.Type != "Organization") || repos[0].Fork || !repos[1].Fork || repos[1].Owner.Type != "User" || !strings.EqualFold(repos[1].Owner.Login, d.actor) || !strings.EqualFold(repos[1].Parent, d.o.Upstream) {
		return errors.New("public personal fork relationship or maintainer permission refused")
	}
	for _, check := range []struct{ root, remote, slug string }{{d.o.UpstreamRoot, "origin", d.o.Upstream}, {d.o.ForkRoot, "origin", d.o.Fork}, {d.o.ForkRoot, "upstream", d.o.Upstream}} {
		info, e := os.Lstat(check.root)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("checkout root refused")
		}
		b, e = d.command(ctx, "checkout identity", "git", "-C", check.root, "rev-parse", "--show-toplevel")
		if e != nil {
			return e
		}
		if filepath.Clean(strings.TrimSpace(string(b))) != check.root {
			return errors.New("checkout root mismatch")
		}
		for _, direction := range [][]string{{}, {"--push"}} {
			argv := []string{"-C", check.root, "remote", "get-url", "--all"}
			argv = append(argv, direction...)
			argv = append(argv, check.remote)
			b, e = d.command(ctx, "remote identity", "git", argv...)
			if e != nil {
				return e
			}
			if remoteSlug(string(b)) != strings.ToLower(check.slug) {
				return errors.New("checkout remote refused")
			}
		}

	}
	return nil
}
func remoteSlug(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "https://github.com/"):
		s = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "git@github.com:"):
		s = strings.TrimPrefix(s, "git@github.com:")
	case strings.HasPrefix(s, "ssh://git@github.com/"):
		s = strings.TrimPrefix(s, "ssh://git@github.com/")
	default:
		return ""
	}
	s = strings.TrimSuffix(s, ".git")
	if !validSlug(s) {
		return ""
	}
	return strings.ToLower(s)
}

type workEnvelope struct {
	Kind   string               `json:"kind"`
	Status string               `json:"status"`
	Result *workflow.WorkResult `json:"result"`
}
type prEnvelope struct {
	Kind   string            `json:"kind"`
	Status string            `json:"status"`
	Result *prservice.Result `json:"result"`
}
type statusEntry struct {
	Repository             string                      `json:"repository"`
	Issue                  *tracker.Issue              `json:"issue"`
	State                  *lease.IssueState           `json:"state"`
	Provenance             []contract.RoleAttestation  `json:"provenance"`
	RoleRuns               []contract.RoleRunTelemetry `json:"role_runs"`
	PlanReplanLimit        int                         `json:"plan_replan_limit"`
	ClaimBudget            *lease.ClaimBudget          `json:"claim_budget,omitempty"`
	ClaimBudgetUnavailable bool                        `json:"claim_budget_unavailable,omitempty"`
	Checkpoint             *workspace.CheckpointState  `json:"checkpoint,omitempty"`
	Repair                 *validation.RepairSummary   `json:"repair,omitempty"`
}

func (d *driver) status(ctx context.Context, r record) (*lease.IssueState, error) {
	b, e := d.swarm(ctx, false, "fresh issue status", "issue", "status", strconv.Itoa(r.Issue))
	if e != nil {
		return nil, e
	}
	var v struct {
		Kind   string        `json:"kind"`
		Issues []statusEntry `json:"issues"`
	}
	if exact(b, &v) != nil || v.Kind != "status" || len(v.Issues) != 1 {
		return nil, errResponse
	}
	s := v.Issues[0]
	if !strings.EqualFold(s.Repository, d.o.Upstream) || s.Issue == nil || s.Issue.Number != r.Issue || s.State == nil || s.Issue.Title != "Public fork regression "+r.ID || !link(s.Issue.HTMLURL, d.o.Upstream, "issues", r.Issue) {
		return nil, errResponse
	}
	if strings.Contains(contract.StripHTMLComments(s.Issue.Body), "swarm-workunit-path:") {
		return nil, errResponse
	}
	wu, e := contract.ParseWorkUnitFromIssue(r.Issue, s.Issue.Title, s.Issue.Body)
	if e != nil {
		return nil, errResponse
	}
	if r.Stage == "revision" && (s.State.PRNumber != r.PR || s.State.PRRevision == nil || s.State.PRRevision.Number != r.PR || s.State.PRRevision.RemoteHeadSHA != r.PublishedHead || !strings.EqualFold(s.State.ForkRepo, d.o.Fork)) {
		return nil, errResponse
	}
	if r.Stage == "issue" && s.State.PRNumber != 0 {
		return nil, errResponse
	}
	if string(s.State.EffectiveGate) != d.o.Gate || lease.VerifyApproval(s.State, wu) != nil {
		return nil, errPaused
	}
	return s.State, nil
}
func (d *driver) approval(ctx context.Context, r record) error {
	_, err := d.status(ctx, r)
	return err
}
func link(s, repo, kind string, n int) bool {
	return n > 0 && s == fmt.Sprintf("https://github.com/%s/%s/%d", repo, kind, n)
}
func checkWork(w *workflow.WorkResult, issue int, phase string) error {
	if w == nil || w.IssueNumber != issue || w.PhaseCompleted != phase || w.Outcome() != "completed" || (phase == "implement" && (!w.ValidationPassed || !shaRE.MatchString(w.HeadSHA))) {
		return errResponse
	}
	return nil
}
func (d *driver) work(ctx context.Context, r record, revision bool, phase string) (string, error) {
	args := []string{"issue", "work", strconv.Itoa(r.Issue), "--phase", phase}
	if revision {
		args = []string{"pr", "work", strconv.Itoa(r.PR), "--phase", phase, "--instructions", "Update only public-fork-regression/" + r.ID + ".txt to contain run ID " + r.ID + " and revision two."}
	}
	b, e := d.swarm(ctx, true, "work "+phase, args...)
	if e != nil {
		return "", e
	}
	if revision {
		var v prEnvelope
		if exact(b, &v) != nil || v.Kind != "pr-work" || v.Status != "pending" || v.Result == nil || v.Result.Status != "pending" || v.Result.IssueNumber != r.Issue {
			return "", errResponse
		}
		if !d.validPR(v.Result.PR, r) {
			return "", errResponse
		}
		if checkWork(v.Result.Work, r.Issue, phase) != nil {
			return "", errResponse
		}
		if phase == "implement" && v.Result.HeadSHA != v.Result.Work.HeadSHA {
			return "", errResponse
		}
		return v.Result.Work.HeadSHA, nil
	}
	var v workEnvelope
	if exact(b, &v) != nil || v.Kind != "work" || v.Status != "completed" || checkWork(v.Result, r.Issue, phase) != nil {
		return "", errResponse
	}
	return v.Result.HeadSHA, nil
}
func (d *driver) prStatus(ctx context.Context, r record, head string) error {
	b, e := d.swarm(ctx, true, "fresh PR status", "pr", "status", strconv.Itoa(r.PR))
	if e != nil {
		return e
	}
	var v prEnvelope
	if exact(b, &v) != nil || v.Kind != "pr-status" || v.Result == nil || v.Status != "pending" || v.Result.Status != "pending" || v.Result.IssueNumber != r.Issue || v.Result.Dirty || v.Result.Rebasing || v.Result.HeadSHA != head || !d.validPR(v.Result.PR, r) {
		return errResponse
	}
	return nil
}
func (d *driver) validPR(p *tracker.PullRequest, r record) bool {
	return p != nil && p.Number == r.PR && p.Draft && !p.Merged && p.State == "open" && link(p.HTMLURL, d.o.Upstream, "pull", r.PR) && strings.EqualFold(p.HeadRepo, d.o.Fork) && strings.EqualFold(p.BaseRepo, d.o.Upstream) && shaRE.MatchString(p.HeadSHA) && (r.PublishedHead == "" || p.HeadSHA == r.PublishedHead)
}

// submissionFreshness mirrors the private CLI emitter in submit_freshness.go.
// It is observation only; publication authority stays inside swarm submit.
type submissionFreshness struct {
	Status          string   `json:"status"`
	BaseDrift       *bool    `json:"base_drift"`
	BehindCount     *int     `json:"behind_count"`
	ConflictedPaths []string `json:"conflicted_paths"`
}

func (d *driver) submit(ctx context.Context, r *record, revision bool, head string) error {
	n := strconv.Itoa(r.Issue)
	if revision {
		if e := d.prStatus(ctx, *r, head); e != nil {
			return e
		}
		n = strconv.Itoa(r.PR)
	} else {
		st, e := d.status(ctx, *r)
		if e != nil {
			return e
		}
		if st.HeadSHA != head {
			return errResponse
		}
	}
	kind := "issue"
	if revision {
		kind = "pr"
	}
	b, e := d.swarm(ctx, true, "explicit draft submit", kind, "submit", n)
	if e != nil {
		return e
	}
	if revision {
		published := *r
		published.PublishedHead = head
		var v prEnvelope
		if exact(b, &v) != nil || v.Kind != "pr-submit" || v.Status != "submitted" || v.Result == nil || v.Result.Status != "submitted" || v.Result.IssueNumber != r.Issue || v.Result.HeadSHA != head || !d.validPR(v.Result.PR, published) || v.Result.PR.HeadSHA != head {
			return errResponse
		}
		return nil
	}
	var v struct {
		Kind     string                          `json:"kind"`
		Status   string                          `json:"status"`
		Issue    int                             `json:"issue_number"`
		PR       int                             `json:"pr_number"`
		URL      string                          `json:"pr_url"`
		Head     string                          `json:"head_sha"`
		Base     *submissionFreshness            `json:"base_freshness,omitempty"`
		Report   *validation.ValidationReport    `json:"report,omitempty"`
		Warnings []validation.AcceptanceWarning  `json:"acceptance_evidence_warnings,omitempty"`
		Harness  *workspace.HarnessPolicyWarning `json:"harness_policy_warning,omitempty"`
	}
	if exact(b, &v) != nil || v.Kind != "submission" || v.Status != "submitted" || v.Issue != r.Issue || v.Head != head || !link(v.URL, d.o.Upstream, "pull", v.PR) {
		return errResponse
	}
	r.PR = v.PR
	r.PublishedHead = head
	fmt.Fprintln(d.out, v.URL)
	// Read the published PR using existing login; do not trust a link as draft proof.
	b, e = d.command(ctx, "published draft", "gh", "api", fmt.Sprintf("repos/%s/pulls/%d", d.o.Upstream, r.PR), "--hostname", "github.com", "--jq", "{number: .number, html_url: .html_url, draft: .draft, head: .head.sha, head_repo: .head.repo.full_name, base_repo: .base.repo.full_name}")
	if e != nil {
		return e
	}
	var p struct {
		Number   int    `json:"number"`
		URL      string `json:"html_url"`
		Draft    bool   `json:"draft"`
		Head     string `json:"head"`
		HeadRepo string `json:"head_repo"`
		BaseRepo string `json:"base_repo"`
	}
	if exact(b, &p) != nil || p.Number != r.PR || !link(p.URL, d.o.Upstream, "pull", r.PR) || !p.Draft || p.Head != head || !strings.EqualFold(p.HeadRepo, d.o.Fork) || !strings.EqualFold(p.BaseRepo, d.o.Upstream) {
		return errResponse
	}
	return nil
}
func (d *driver) runLifecycle(ctx context.Context) error {
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return errors.New("run identifier unavailable")
	}
	r := record{Options: d.o, Actor: d.actor, ID: hex.EncodeToString(nonce[:]), Stage: "issue"}
	path := "public-fork-regression/" + r.ID + ".txt"
	b, e := d.swarm(ctx, false, "create fixture", "issue", "create", "--title", "Public fork regression "+r.ID, "--objective", "Create only "+path+" containing run ID "+r.ID+" and revision one.", "--constraint", "Change only "+path, "--acceptance", path+" contains run ID "+r.ID+".", "--workflow", "feature", "--gate", d.o.Gate)
	if e != nil {
		return e
	}
	var created struct {
		Kind   string         `json:"kind"`
		Status string         `json:"status"`
		Issue  *tracker.Issue `json:"issue"`
		Allow  bool           `json:"allow_harness_changes"`
	}
	if exact(b, &created) != nil || created.Kind != "issue" || created.Status != "draft" || created.Allow || created.Issue == nil || !link(created.Issue.HTMLURL, d.o.Upstream, "issues", created.Issue.Number) {
		return errResponse
	}
	r.Issue = created.Issue.Number
	fmt.Fprintln(d.out, created.Issue.HTMLURL)
	for _, op := range []string{"approve", "claim"} {
		b, e = d.swarm(ctx, op == "claim", op, "issue", op, strconv.Itoa(r.Issue))
		if e != nil {
			return e
		}
		if op == "approve" {
			var v struct {
				Kind       string               `json:"kind"`
				Issue      int                  `json:"issue_number"`
				State      *lease.IssueState    `json:"state"`
				Dimensions *workflow.Dimensions `json:"suggested_dimensions,omitempty"`
				Signals    []string             `json:"signals,omitempty"`
				Hints      []string             `json:"hints,omitempty"`
			}
			if exact(b, &v) != nil || v.Kind != "task-approval" || v.Issue != r.Issue || v.State == nil {
				return errResponse
			}
		} else {
			var v struct {
				Kind     string            `json:"kind"`
				Issue    int               `json:"issue_number"`
				State    *lease.IssueState `json:"state"`
				Branch   string            `json:"branch"`
				Worktree string            `json:"worktree"`
			}
			if exact(b, &v) != nil || v.Kind != "claim" || v.Issue != r.Issue || v.State == nil || v.Branch == "" || v.Worktree == "" {
				return errResponse
			}
		}
	}
	if _, e = d.work(ctx, r, false, "plan"); e != nil {
		return e
	}
	return d.continueGate(ctx, r, false)
}
func (d *driver) continueGate(ctx context.Context, r record, resuming bool) error {
	if !resuming && d.o.Gate == "agent" {
		b, e := d.swarm(ctx, false, "trusted plan review", "issue", "review-plan", strconv.Itoa(r.Issue))
		if e != nil {
			return e
		}
		var v workEnvelope
		if exact(b, &v) != nil || v.Kind != "work" || v.Result == nil || v.Result.IssueNumber != r.Issue {
			return errResponse
		}
		if v.Status == "paused" && v.Result.Outcome() == "paused" && v.Result.PhaseCompleted == "plan" {
			return d.pause(r)
		}
		if v.Status != "completed" || v.Result.Outcome() != "completed" || v.Result.PhaseCompleted != "review" {
			return errResponse
		}
	}
	if !resuming && d.o.Gate == "human" {
		return d.pause(r)
	}
	if e := d.approval(ctx, r); e != nil {
		if errors.Is(e, errPaused) && !resuming {
			return d.pause(r)
		}
		return e
	}
	revision := r.Stage == "revision"
	head, e := d.work(ctx, r, revision, "implement")
	if e != nil {
		return e
	}
	if e = d.submit(ctx, &r, revision, head); e != nil {
		return e
	}
	if revision {
		r.Stage = "completed"
		r.PublishedHead = head
		if e := d.saveRecord(r); e != nil {
			return e
		}
		fmt.Fprintln(d.out, "completed: retained draft and fixtures")
		return nil
	}
	r.Stage = "revision"
	if _, e = d.work(ctx, r, true, "plan"); e != nil {
		return e
	}
	return d.continueGate(ctx, r, false)
}
func (d *driver) saveRecord(r record) error {
	r.Options.Resume = ""
	r.Options.DryRun = false
	// An exclusive private directory owns immutable stage records. safefs rejects
	// links and special files and atomically replaces through exclusive temp files.
	parent, e := os.OpenRoot(d.o.ForkRoot)
	if e != nil {
		return errors.New("record root unavailable")
	}
	defer parent.Close()
	dirname := ".swarm-public-fork-" + r.ID + "-" + r.Stage
	if e = parent.Mkdir(dirname, 0700); e != nil {
		return errors.New("exclusive record directory unavailable")
	}
	bounded, e := safefs.Open(d.o.ForkRoot)
	if e != nil {
		return errors.New("record root refused")
	}
	defer bounded.Close()
	root, e := bounded.Sub(dirname, false, 0)
	if e != nil {
		return errors.New("record directory refused")
	}
	defer root.Close()
	b, e := json.Marshal(r)
	if e != nil {
		return errors.New("record encoding failed")
	}
	if e = root.WriteFile("resume.json", b, 0600); e != nil {
		return errors.New("record write failed")
	}
	// Print a relative record name; absolute checkout paths never enter summaries.
	fmt.Fprintln(d.out, "retained record relative to fork checkout: "+dirname+"/resume.json")
	return nil
}
func (d *driver) pause(r record) error {
	if e := d.saveRecord(r); e != nil {
		return e
	}
	fmt.Fprintf(d.out, "Independent maintainer: swarm --backend github --repo %s --root 'UPSTREAM_CHECKOUT' --upstream-remote origin issue review-plan %d --approve --notes '<maintainer-decision>'\n", d.o.Upstream, r.Issue)
	return errPaused
}

// Fail closed on platforms without a uid-bearing ownership result.
func owned(info os.FileInfo) bool {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		stat = stat.Elem()
	}
	if !stat.IsValid() || stat.Kind() != reflect.Struct {
		return false
	}
	uid := stat.FieldByName("Uid")
	return uid.IsValid() && uid.Kind() == reflect.Uint32 && uid.Uint() == uint64(os.Getuid())
}
func (d *driver) resumeLifecycle(ctx context.Context) error {
	path, e := filepath.Abs(d.o.Resume)
	if e != nil {
		return errors.New("record refused")
	}
	dir := filepath.Dir(path)
	base := filepath.Base(dir)
	if filepath.Dir(dir) != d.o.ForkRoot || filepath.Base(path) != "resume.json" || !strings.HasPrefix(base, ".swarm-public-fork-") {
		return errors.New("record outside fixture checkout")
	}
	root, e := safefs.Open(d.o.ForkRoot)
	if e != nil {
		return errors.New("record refused")
	}
	defer root.Close()
	dirHandle, e := root.OpenDirectory(base)
	if e != nil {
		return errors.New("record directory refused")
	}
	defer dirHandle.Close()
	dirInfo, e := dirHandle.Stat()
	if e != nil || dirInfo.Mode().Perm() != 0700 || !owned(dirInfo) {
		return errors.New("record directory ownership or permissions refused")
	}
	f, e := root.OpenRegular(filepath.Join(base, "resume.json"))
	if e != nil {
		return errors.New("record refused")
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || info.Mode().Perm() != 0600 {
		return errors.New("record permissions refused")
	}
	if !owned(info) {
		return errors.New("foreign record refused")
	}
	b, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil || len(b) > 16384 {
		return errors.New("record refused")
	}
	var r record
	if exact(b, &r) != nil || !idRE.MatchString(r.ID) || r.Issue <= 0 || (r.Stage != "issue" && r.Stage != "revision") || (r.Stage == "issue" && (r.PR != 0 || r.PublishedHead != "")) || (r.Stage == "revision" && (r.PR <= 0 || !shaRE.MatchString(r.PublishedHead))) || base != ".swarm-public-fork-"+r.ID+"-"+r.Stage || r.Actor != d.actor {
		return errors.New("record identity refused")
	}
	expected := d.o
	expected.Resume = ""
	expected.DryRun = false
	if r.Options != expected {
		return errors.New("record parameters mismatch")
	}
	return d.continueGate(ctx, r, true)
}
func (d *driver) preview() {
	fmt.Fprintln(d.out, "Dry run: public relationship verification pending live preflight. No subprocesses or writes.")
	fmt.Fprintln(d.out, "gh api user; gh api repos/<upstream>; gh api repos/<fork>; git checkout/remote identity checks")
	for _, s := range []string{"upstream issue create --title <unique-title> --objective <unique-file> --constraint <unique-file-only> --acceptance <file-result> --workflow feature --gate " + d.o.Gate + " --json", "upstream issue approve <issue> --json", "contributor issue claim <issue> --json", "contributor issue work <issue> --phase plan --json", "upstream issue review-plan <issue> --json (agent) OR manual independent approval pause (human)", "upstream issue status <issue> --json; verify exact approval and gate", "contributor issue work <issue> --phase implement --json", "upstream issue status <issue> --json; require <candidate-head>", "contributor issue submit <issue> --json; verify draft <PR> and <candidate-head>", "contributor pr work <PR> --phase plan --instructions <unique-file-revision> --json", "upstream review or manual pause; fresh issue status and approval verification", "contributor pr work <PR> --phase implement --instructions <unique-file-revision> --json", "contributor pr status <PR> --json; require <revision-head>", "contributor pr submit <PR> --json; require same PR and <revision-head>"} {
		fmt.Fprintln(d.out, s)
	}
	fmt.Fprintf(d.out, "Every swarm command: --backend github --repo %s --root <checkout> --upstream-remote origin (upstream) or upstream (contributor); contributor also --fork --fork-remote origin. Resume dry-run never reads or advances a record.\n", d.o.Upstream)
}
