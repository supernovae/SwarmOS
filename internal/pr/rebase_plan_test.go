package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/agent"
	"github.com/supernovae/swarmos/internal/lease"
)

func TestRebasePlanningOutputIsBoundedAndReadOnly(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"valid", "omitted path", "missing side", "source edit", "context edit", "source deletion", "symlink", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			put := func(p, body string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			put("file.txt", "conflicted\n")
			put(".swarm/active/PLAN.md", "approved behavior")
			before, err := snapshotPlanTree(dir)
			if err != nil {
				t.Fatal(err)
			}
			body := "## file.txt\nOurs: upstream\nTheirs: PR\nResolution: combine\nApproved behavior: retained\n"
			switch kind {
			case "omitted path":
				body = strings.ReplaceAll(body, "file.txt", "another.txt")
			case "missing side":
				body = strings.ReplaceAll(body, "Theirs: PR", "Theirs:")
			case "source edit":
				put("file.txt", "edited")
			case "context edit":
				put(".swarm/active/PLAN.md", "changed")
			case "source deletion":
				if err := os.Remove(filepath.Join(dir, "file.txt")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("file.txt", filepath.Join(dir, "link")); err != nil {
					t.Skip(err)
				}
			case "oversize":
				body = strings.Repeat("x", (64<<10)+1)
			}
			put(".swarm/active/REBASE_PLAN.md", body)
			_, err = verifyRebasePlan(dir, before, []string{"file.txt"})
			if (err == nil) != (kind == "valid") {
				t.Fatalf("%s: %v", kind, err)
			}
		})
	}
}

func TestRebasePlanLabelForms(t *testing.T) {
	t.Parallel()
	// PR #241 was rejected for the bold and next-line forms (assumed from the
	// issue report; its REBASE_PLAN.md is not available to confirm).
	accepted := map[string]func(label, text string) string{
		"plain":                 func(l, x string) string { return l + ": " + x },
		"bold colon inside":     func(l, x string) string { return "**" + l + ":** " + x },
		"bold colon outside":    func(l, x string) string { return "**" + l + "**: " + x },
		"underscore bold":       func(l, x string) string { return "__" + l + ":__ " + x },
		"dash list":             func(l, x string) string { return "- " + l + ": " + x },
		"star list":             func(l, x string) string { return "* " + l + ": " + x },
		"numbered list":         func(l, x string) string { return "1. " + l + ": " + x },
		"bold list":             func(l, x string) string { return "- **" + l + ":** " + x },
		"next line":             func(l, x string) string { return l + ":\n" + x },
		"bold list, continued":  func(l, x string) string { return "- **" + l + ":**\n  " + x + "\n  and more" },
		"lowercase label":       func(l, x string) string { return strings.ToLower(l) + ": " + x },
		"blank line, then text": func(l, x string) string { return l + ":\n\n" + x },
	}
	labels := []string{"Ours", "Theirs", "Resolution", "Approved behavior"}
	for name, form := range accepted {
		t.Run(name, func(t *testing.T) {
			var body strings.Builder
			body.WriteString("## file.txt\n")
			for _, l := range labels {
				body.WriteString(form(l, "text for "+l) + "\n")
			}
			if err := checkPlanBody(t, body.String()); err != nil {
				t.Fatalf("%q rejected: %v", body.String(), err)
			}
		})
	}
	rest := "Theirs: PR\nResolution: combine\nApproved behavior: retained\n"
	for name, body := range map[string]string{
		"empty before next label":  "## file.txt\nOurs:\n" + rest,
		"empty before heading":     "## file.txt\n" + rest + "Ours:\n### note\nlater text\n",
		"emphasis only":            "## file.txt\n**Ours:** **\n" + rest,
		"last line of the section": "## file.txt\n" + rest + "Ours:\n",
		"label missing":            "## file.txt\n" + rest,
		"label in another section": "## other.txt\nOurs: upstream\n\n## file.txt\n" + rest,
		"empty before next path":   "## file.txt\n" + rest + "Ours:\n## other.txt\nnot this path\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := checkPlanBody(t, body)
			if err == nil || !strings.Contains(err.Error(), "rebase plan lacks Ours: for file.txt") {
				t.Fatalf("%q: err = %v", body, err)
			}
		})
	}
}

func checkPlanBody(t *testing.T, body string) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".swarm", "active"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("conflicted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".swarm", "active", "PLAN.md"), []byte("plan"), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotPlanTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".swarm", "active", "REBASE_PLAN.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = verifyRebasePlan(dir, before, []string{"file.txt"})
	return err
}

func TestMicroPlanErrorClassification(t *testing.T) {
	t.Parallel()
	valid := "## file.txt\nOurs: upstream\nTheirs: PR\nResolution: combine\nApproved behavior: retained\n"
	for _, tc := range []struct {
		name      string
		body      string
		mutate    func(dir string) error
		malformed bool
	}{
		{name: "missing plan", malformed: true},
		{name: "omitted path", body: strings.ReplaceAll(valid, "file.txt", "other.txt"), malformed: true},
		{name: "empty label", body: strings.Replace(valid, "Ours: upstream", "Ours:", 1), malformed: true},
		{name: "changed file", body: valid, mutate: func(dir string) error { return os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0644) }},
		{name: "removed file", body: valid, mutate: func(dir string) error { return os.Remove(filepath.Join(dir, "file.txt")) }},
		{name: "new directory", body: valid, mutate: func(dir string) error { return os.Mkdir(filepath.Join(dir, "newdir"), 0755) }},
		{name: "oversized plan", body: strings.Repeat("x", (64<<10)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("conflicted\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, ".swarm", "active"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".swarm", "active", "PLAN.md"), []byte("plan"), 0644); err != nil {
				t.Fatal(err)
			}
			before, err := snapshotPlanTree(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.body != "" {
				if err := os.WriteFile(filepath.Join(dir, ".swarm", "active", "REBASE_PLAN.md"), []byte(tc.body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mutate != nil {
				if err := tc.mutate(dir); err != nil {
					t.Fatal(err)
				}
			}
			_, err = verifyRebasePlan(dir, before, []string{"file.txt"})
			var malformed *microPlanError
			if err == nil || errors.As(err, &malformed) != tc.malformed {
				t.Fatalf("err = %v (%T), want malformed=%v", err, err, tc.malformed)
			}
		})
	}
}

// planScript writes the nth planning turn's REBASE_PLAN.md ("" writes none)
// and records each task prompt.
type planScript struct {
	*agent.MockRunner
	plans   []string
	prompts []string
}

func (p *planScript) Run(_ context.Context, req agent.RunRequest) (*agent.RunResult, error) {
	n := len(p.prompts)
	p.prompts = append(p.prompts, req.TaskPrompt)
	if n < len(p.plans) && p.plans[n] != "" {
		if err := os.WriteFile(filepath.Join(req.WorktreePath, ".swarm", "active", "REBASE_PLAN.md"), []byte(p.plans[n]), 0644); err != nil {
			return nil, err
		}
	}
	return &agent.RunResult{Agent: "mock", Role: req.Role}, nil
}

func TestPlanRebaseStopRetriesMalformedPlanOnce(t *testing.T) {
	t.Parallel()
	valid := "## file.txt\nOurs: upstream\nTheirs: PR\nResolution: combine\nApproved behavior: retained\n"
	malformed := strings.Replace(valid, "Ours: upstream", "Ours:", 1)
	for _, tc := range []struct {
		name  string
		fork  bool
		plans []string
		turns int
		// accepted means the plan passed and planning reached the fenced update.
		accepted bool
	}{
		{name: "malformed then valid", plans: []string{malformed, valid}, turns: 2, accepted: true},
		{name: "malformed twice", plans: []string{malformed, malformed}, turns: 2},
		{name: "missing then valid", plans: []string{"", valid}, turns: 2, accepted: true},
		{name: "second turn writes nothing", plans: []string{malformed, ""}, turns: 2},
		{name: "oversized is not retried", plans: []string{strings.Repeat("x", (64<<10)+1), valid}, turns: 1},
		{name: "fork mode is not retried", fork: true, plans: []string{malformed, valid}, turns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".swarm", "active"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("conflicted\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runner := &planScript{MockRunner: &agent.MockRunner{}, plans: tc.plans}
			rv := &resolver{runner: runner, agentName: "mock", instructions: "keep upstream imports"}
			// No fenced rebase context: an accepted plan stops right before recording.
			_, err := (&Service{Fork: tc.fork}).planRebaseStop(context.Background(), rv, dir, 1, "alice", strings.Repeat("a", 40), []string{"file.txt"}, &lease.IssueState{})
			if !errors.Is(err, ErrResolveRejected) {
				t.Fatalf("err = %v", err)
			}
			if got := strings.Contains(err.Error(), "missing fenced rebase context"); got != tc.accepted {
				t.Fatalf("accepted = %v, want %v: %v", got, tc.accepted, err)
			}
			if len(runner.prompts) != tc.turns {
				t.Fatalf("planning turns = %d, want %d", len(runner.prompts), tc.turns)
			}
			for _, prompt := range runner.prompts {
				if !strings.HasPrefix(prompt, rebasePlanTask) || !strings.Contains(prompt, "Operator instructions: keep upstream imports") {
					t.Fatalf("planning prompt = %q", prompt)
				}
			}
			if tc.turns == 2 && !strings.Contains(runner.prompts[1], "The previous REBASE_PLAN.md was rejected: ") {
				t.Fatalf("retry prompt lacks the reason: %q", runner.prompts[1])
			}
			if tc.turns == 2 && tc.plans[0] == malformed && !strings.Contains(runner.prompts[1], "rebase plan lacks Ours: for file.txt") {
				t.Fatalf("retry prompt lacks the reason: %q", runner.prompts[1])
			}
		})
	}
}
