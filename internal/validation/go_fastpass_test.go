package validation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/progress"
)

// goModuleFixture is a small offline module: leaf is imported by consumer
// (and transitively by transitive), by the internal tests of testonly, and by
// the external tests of xtest. unrelated imports nothing.
var goModuleFixture = map[string]string{
	"go.mod":                         "module example.com/m\n\ngo 1.21\n",
	"leaf/leaf.go":                   "package leaf\n\nfunc Value() int { return 1 }\n",
	"leaf/leaf_test.go":              "package leaf\n\nimport \"testing\"\n\nfunc TestLeaf(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal(\"leaf changed\")\n\t}\n}\n",
	"consumer/consumer.go":           "package consumer\n\nimport \"example.com/m/leaf\"\n\nfunc Double() int { return 2 * leaf.Value() }\n",
	"consumer/consumer_test.go":      "package consumer\n\nimport \"testing\"\n\nfunc TestConsumer(t *testing.T) { _ = Double() }\n",
	"transitive/transitive.go":       "package transitive\n\nimport \"example.com/m/consumer\"\n\nfunc Quad() int { return 2 * consumer.Double() }\n",
	"testonly/testonly.go":           "package testonly\n",
	"testonly/testonly_test.go":      "package testonly\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/leaf\"\n)\n\nfunc TestUsesLeaf(t *testing.T) { _ = leaf.Value() }\n",
	"xtest/xtest.go":                 "package xtest\n",
	"xtest/xtest_test.go":            "package xtest_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/leaf\"\n)\n\nfunc TestExternal(t *testing.T) { _ = leaf.Value() }\n",
	"unrelated/unrelated.go":         "package unrelated\n",
	"unrelated/unrelated_test.go":    "package unrelated\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {}\n",
	"docs/notes.md":                  "notes\n",
	".swarm/config.yaml":             "version: 1\ndefault_agent: mock\ndefault_gate: none\nexecution:\n  mode: trusted-local\nlease:\n  duration: 1h\n  max_renewals: 1\nsecurity:\n  require_ready_label: true\n  ready_label: swarm:ready\nagents:\n  mock:\n    binary: git\n    harness: mock\n",
	".github/workflows/validate.yml": "on: push\n",
}

var affectedByLeaf = []string{"example.com/m/consumer", "example.com/m/leaf", "example.com/m/testonly", "example.com/m/transitive", "example.com/m/xtest"}

// setupGoFastPassRepo commits the module fixture with the given trusted
// checks and returns the repository and its approved base.
func setupGoFastPassRepo(t *testing.T, checks string, overrides map[string]string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	files := map[string]string{".swarm/validation/required-checks.yaml": "required_checks:\n" + checks + "protected_paths:\n  - .github/workflows/**\n  - .swarm/validation/**\n"}
	for name, contents := range goModuleFixture {
		files[name] = contents
	}
	for name, contents := range overrides {
		files[name] = contents
	}
	for name, contents := range files {
		writeRepoFile(t, repo, name, contents)
	}
	runRepoGit(t, repo, "init", "-b", "main")
	runRepoGit(t, repo, "config", "user.name", "Test")
	runRepoGit(t, repo, "config", "user.email", "test@localhost")
	runRepoGit(t, repo, "add", ".")
	runRepoGit(t, repo, "commit", "-m", "base")
	writeRepoFile(t, repo, ".swarm/active/PLAN.md", "# Plan\n")
	writeRepoFile(t, repo, ".swarm/active/TEST_PLAN.md", "# Test Plan\n")
	return repo, gitRevision(t, repo, "HEAD")
}

func writeRepoFile(t *testing.T, repo, name, contents string) {
	t.Helper()
	full := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runRepoGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return string(out)
}

func commitRepoFile(t *testing.T, repo, name, contents string) {
	t.Helper()
	writeRepoFile(t, repo, name, contents)
	runRepoGit(t, repo, "add", name)
	runRepoGit(t, repo, "commit", "-m", "change "+name)
}

const failingLeaf = "package leaf\n\nfunc Value() int { return 2 }\n"
const passingLeaf = "package leaf\n\nfunc Value() int { return 1 + 0 }\n"

func recordEvents(events *[]string) func(progress.Event) {
	return func(e progress.Event) { *events = append(*events, e.Check+" "+e.Status) }
}

func validateFastPass(t *testing.T, ctx context.Context, repo, base string, events *[]string) (*ValidationReport, error) {
	t.Helper()
	return Validate(ctx, repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base, GoFastPass: true, OnProgress: recordEvents(events)})
}

func TestGoFastPassFailingChangedPackage(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n  - name: after\n    command: \"touch after-ran\"\n    timeout: 1m\n", nil)
	commitRepoFile(t, repo, "leaf/leaf.go", failingLeaf)
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if !errors.Is(err, ErrRequiredCheckFailed) || report == nil || report.Passed {
		t.Fatalf("changed-package failure must fail validation: %+v %v", report, err)
	}
	if len(report.CheckOutcomes) != 1 {
		t.Fatalf("full suite or later checks ran: %+v", report.CheckOutcomes)
	}
	pre := report.CheckOutcomes[0]
	if pre.Name != "test (changed packages)" || !pre.Preliminary || pre.Passed || pre.InfrastructureError || !slices.Equal(pre.Packages, affectedByLeaf) {
		t.Fatalf("preliminary outcome: %+v", pre)
	}
	if !slices.Contains(pre.FailingTests, "TestLeaf") || !strings.Contains(pre.Output, "example.com/m/leaf") || strings.Contains(pre.Command, "./...") || strings.Contains(pre.Output, "unrelated") {
		t.Fatalf("failure must name the changed package without running others: %+v", pre)
	}
	if !slices.Equal(events, []string{"test (changed packages) started", "test (changed packages) FAIL"}) {
		t.Fatalf("events: %v", events)
	}
	if _, err := os.Stat(filepath.Join(repo, "after-ran")); !os.IsNotExist(err) {
		t.Fatalf("later required check ran after the early failure: %v", err)
	}
	if report.Artifact.Verdict != "FAIL" || report.Artifact.Metadata["validation_report"] != "" || !strings.Contains(report.Artifact.Summary, "changed packages failed") {
		t.Fatalf("artifact: %+v", report.Artifact)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".swarm", "active", "VALIDATION.md"))
	if err != nil || !strings.Contains(string(body), "test (changed packages)") || !strings.Contains(string(body), "2 required check(s) did not run") {
		t.Fatalf("validation report: %s %v", body, err)
	}
}

func TestGoFastPassThenFull(t *testing.T) {
	const command = "go test -count=1 ./..."
	checks := "  - name: test\n    command: " + command + "\n    timeout: 5m\n  - name: ok\n    command: \"true\"\n    timeout: 1m\n"
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		pass      bool
	}{
		{"full passes", nil, true},
		// The unrelated failure is outside the changed-package selection, so
		// only the full run can find it.
		{"full fails", map[string]string{"unrelated/unrelated_test.go": "package unrelated\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) { t.Fatal(\"broken\") }\n"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := setupGoFastPassRepo(t, checks, tc.overrides)
			commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
			var events []string
			report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, ValidateOptions{BaseSHA: base, GoFastPass: true, Version: "v1.0.0", ClaimID: "claim", OnProgress: recordEvents(&events)})
			if (err == nil) != tc.pass || report == nil || report.Passed != tc.pass {
				t.Fatalf("full run must decide: %+v %v", report, err)
			}
			if len(report.CheckOutcomes) != 3 {
				t.Fatalf("outcomes: %+v", report.CheckOutcomes)
			}
			pre, full := report.CheckOutcomes[0], report.CheckOutcomes[1]
			if !pre.Preliminary || !pre.Passed || !slices.Equal(pre.Packages, affectedByLeaf) {
				t.Fatalf("preliminary: %+v", pre)
			}
			if full.Name != "test" || full.Command != command || full.Preliminary || full.Passed != tc.pass {
				t.Fatalf("full check must run unchanged: %+v", full)
			}
			want := []string{"test (changed packages) started", "test (changed packages) PASS", "test started", "test PASS", "ok started", "ok PASS"}
			if !tc.pass {
				want[3] = "test FAIL"
			}
			if !slices.Equal(events, want) {
				t.Fatalf("events: %v", events)
			}
			if tc.pass {
				if report.Artifact.Summary != "All 2 required checks passed" || report.Artifact.Metadata["validation_report"] == "" {
					t.Fatalf("passing artifact: %+v", report.Artifact)
				}
			} else if report.Artifact.Verdict != "FAIL" || report.Artifact.Metadata["validation_report"] != "" {
				t.Fatalf("failed full run produced reusable evidence: %+v", report.Artifact)
			}
		})
	}
}

func TestGoFastPassNoGoChanges(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n", nil)
	commitRepoFile(t, repo, "docs/notes.md", "changed notes\n")
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CheckOutcomes) != 1 || report.CheckOutcomes[0].Preliminary || !slices.Equal(events, []string{"test started", "test PASS"}) {
		t.Fatalf("documentation change must not add a preliminary run: %+v %v", report.CheckOutcomes, events)
	}
	if strings.Contains(report.Artifact.Body, "Changed-package") {
		t.Fatalf("ordinary skip must not be reported: %s", report.Artifact.Body)
	}
}

func TestGoFastPassNonGoCheck(t *testing.T) {
	checks := "  - name: vet\n    command: go vet ./...\n    timeout: 5m\n  - name: pattern\n    command: go test -count=1 ./leaf/...\n    timeout: 5m\n  - name: shell\n    command: \"true\"\n    timeout: 1m\n"
	repo, base := setupGoFastPassRepo(t, checks, nil)
	commitRepoFile(t, repo, "leaf/leaf.go", failingLeaf)
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if !errors.Is(err, ErrRequiredCheckFailed) {
		t.Fatalf("leaf tests should fail in the pattern check: %v", err)
	}
	var names []string
	for _, outcome := range report.CheckOutcomes {
		names = append(names, outcome.Name+"="+outcome.Command)
	}
	if !slices.Equal(names, []string{"vet=go vet ./...", "pattern=go test -count=1 ./leaf/...", "shell=true"}) {
		t.Fatalf("ineligible checks must run exactly as configured: %v", names)
	}
}

func TestGoFastPassOptOutAndCIMode(t *testing.T) {
	for name, opts := range map[string]ValidateOptions{
		"opt-out": {},
		"CI":      {CIMode: true, GoFastPass: true},
	} {
		t.Run(name, func(t *testing.T) {
			repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n", nil)
			commitRepoFile(t, repo, "leaf/leaf.go", failingLeaf)
			opts.BaseSHA = base
			report, err := Validate(t.Context(), repo, &contract.WorkUnit{ID: "ISSUE-1", IssueNumber: 1}, opts)
			if !errors.Is(err, ErrRequiredCheckFailed) || len(report.CheckOutcomes) != 1 || report.CheckOutcomes[0].Preliminary {
				t.Fatalf("only the full check may run: %+v %v", report, err)
			}
		})
	}
}

func TestGoFastPassDirtyIssueValidate(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n", nil)
	// HEAD stays at the base: the failure is an unstaged edit, alongside a
	// new untracked package.
	writeRepoFile(t, repo, "leaf/leaf.go", failingLeaf)
	writeRepoFile(t, repo, "extra/extra.go", "package extra\n")
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if !errors.Is(err, ErrRequiredCheckFailed) || len(report.CheckOutcomes) != 1 {
		t.Fatalf("dirty edit must fail early: %+v %v", report, err)
	}
	want := append(slices.Clone(affectedByLeaf), "example.com/m/extra")
	slices.Sort(want)
	if pre := report.CheckOutcomes[0]; !pre.Preliminary || !slices.Equal(pre.Packages, want) {
		t.Fatalf("checkpoint selection: %v", pre.Packages)
	}
	if staged := runRepoGit(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("checkpoint capture changed the index: %q", staged)
	}
	if status := runRepoGit(t, repo, "status", "--porcelain", "--", "leaf", "extra"); !strings.Contains(status, " M leaf/leaf.go") || !strings.Contains(status, "?? extra/") {
		t.Fatalf("checkpoint capture changed the worktree: %q", status)
	}
}

func TestGoFastPassModuleChangesUseFullRun(t *testing.T) {
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n", nil)
	writeRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	writeRepoFile(t, repo, "go.mod", "module example.com/m\n\ngo 1.22\n")
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.CheckOutcomes) != 1 || !strings.Contains(report.Artifact.Body, "skipped: module configuration changed") {
		t.Fatalf("module change must fall back to the full run: %+v %s", report.CheckOutcomes, report.Artifact.Body)
	}
}

// installFakeGo puts a scripted go command first on PATH. It logs each
// invocation and serves package metadata for the module fixture.
func installFakeGo(t *testing.T, testScript string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	metadata := `{"ImportPath":"example.com/m/leaf","Dir":"/m/leaf","Module":{"Path":"example.com/m","Dir":"/m","Main":true}}
{"ImportPath":"example.com/m/consumer","Dir":"/m/consumer","Module":{"Path":"example.com/m","Dir":"/m","Main":true},"Imports":["example.com/m/leaf"]}
{"ImportPath":"example.com/m/unrelated","Dir":"/m/unrelated","Module":{"Path":"example.com/m","Dir":"/m","Main":true}}
`
	writeRepoFile(t, dir, "metadata.json", metadata)
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\ncase \"$1\" in\nlist) echo 'go: noise on stderr' >&2; cat '" + filepath.Join(dir, "metadata.json") + "' ;;\ntest)\n" + testScript + "\n;;\nesac\n"
	writeRepoFile(t, dir, "go", script)
	if err := os.Chmod(filepath.Join(dir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// readLog returns the check commands the fake go saw. The coordinator's
// dependency prefetch also runs go on the host; those calls are dropped.
func readLog(t *testing.T, log string) []string {
	t.Helper()
	data, _ := os.ReadFile(log)
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "env GOVERSION" && !strings.HasPrefix(line, "mod download") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestGoFastPassProgress(t *testing.T) {
	log := installFakeGo(t, `case "$*" in *./...*) exit 0 ;; esac; echo "ok $*"`)
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -race ./...\n    timeout: 1m\n", nil)
	commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	var events []string
	if _, err := validateFastPass(t, t.Context(), repo, base, &events); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"test (changed packages) started", "test (changed packages) PASS", "test started", "test PASS"}) {
		t.Fatalf("events: %v", events)
	}
	want := []string{
		"list -e -json=ImportPath,Dir,Module,Imports,TestImports,XTestImports -race ./...",
		"test -race example.com/m/consumer example.com/m/leaf",
		"test -race ./...",
	}
	if got := readLog(t, log); !slices.Equal(got, want) {
		t.Fatalf("invocations: %q", got)
	}
}

func TestGoFastPassInfrastructureFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		full     string
		pass     bool
		fallback string
	}{
		// The preliminary run uses its whole allowance; the full run then gets
		// a fresh timeout, so finishing after the old deadline still passes.
		{"timeout then full pass", "sleep 1; exit 0", true, "timed out"},
		{"timeout then full fail", "exit 3", false, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := installFakeGo(t, `case "$*" in *./...*) `+tc.full+` ;; esac; sleep 30`)
			repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test ./...\n    timeout: 1500ms\n", nil)
			commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
			var events []string
			report, err := validateFastPass(t, t.Context(), repo, base, &events)
			if (err == nil) != tc.pass || report.Passed != tc.pass {
				t.Fatalf("full run must decide: %+v %v", report, err)
			}
			pre, full := report.CheckOutcomes[0], report.CheckOutcomes[1]
			if !pre.Preliminary || !strings.Contains(pre.FallbackReason, tc.fallback) || pre.InfrastructureError {
				t.Fatalf("fallback must be a non-gating diagnostic: %+v", pre)
			}
			if full.Passed != tc.pass || full.Command != "go test ./..." {
				t.Fatalf("full: %+v", full)
			}
			if tc.pass && report.Artifact.Metadata["validation_report"] != "" {
				t.Fatalf("no reuse context was supplied: %+v", report.Artifact.Metadata)
			}
			if events[1] != "test (changed packages) skipped" || events[2] != "test started" {
				t.Fatalf("events: %v", events)
			}
			if got := readLog(t, log); len(got) != 3 || got[2] != "test ./..." {
				t.Fatalf("invocations: %q", got)
			}
		})
	}
}

func TestGoFastPassCancellationStartsNoFullRun(t *testing.T) {
	log := installFakeGo(t, `sleep 30`)
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test ./...\n    timeout: 1m\n", nil)
	commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var events []string
	report, err := Validate(ctx, repo, nil, ValidateOptions{BaseSHA: base, GoFastPass: true, OnProgress: func(e progress.Event) {
		events = append(events, e.Check+" "+e.Status)
		if e.Status == "started" {
			cancel()
		}
	}})
	if err == nil || report == nil || report.Passed || len(report.CheckOutcomes) != 1 {
		t.Fatalf("cancellation: %+v %v", report, err)
	}
	if !slices.Equal(events, []string{"test (changed packages) started", "test (changed packages) FAIL"}) {
		t.Fatalf("events: %v", events)
	}
	for _, line := range readLog(t, log) {
		if strings.HasPrefix(line, "test") && strings.Contains(line, "./...") {
			t.Fatalf("full run started after cancellation: %q", line)
		}
	}
}

func TestGoFastPassReverseDependencies(t *testing.T) {
	module := func(path, dir string, imports, testImports, xtestImports []string) goPackage {
		pkg := goPackage{ImportPath: path, Dir: dir, Imports: imports, TestImports: testImports, XTestImports: xtestImports}
		return pkg
	}
	packages := []goPackage{
		module("m/leaf", "leaf", []string{"fmt"}, nil, nil),
		module("m/consumer", "consumer", []string{"m/leaf"}, nil, nil),
		module("m/transitive", "transitive", []string{"m/consumer"}, nil, nil),
		module("m/testonly", "testonly", nil, []string{"m/leaf"}, nil),
		module("m/xtest", "xtest", nil, nil, []string{"m/xtest", "m/leaf"}),
		module("m/a", "a", []string{"m/b"}, nil, nil),
		module("m/b", "b", []string{"m/a"}, nil, nil),
		module("m/unrelated", "unrelated", []string{"example.org/other"}, nil, nil),
		module("m", ".", nil, nil, nil),
	}
	for _, tc := range []struct {
		dirs []string
		want []string
	}{
		{[]string{"leaf", "leaf"}, []string{"m/consumer", "m/leaf", "m/testonly", "m/transitive", "m/xtest"}},
		{[]string{"consumer"}, []string{"m/consumer", "m/transitive"}},
		{[]string{"a"}, []string{"m/a", "m/b"}},
		{[]string{"."}, []string{"m"}},
	} {
		got, err := affectedPackages(packages, tc.dirs)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("affectedPackages(%v) = %v, %v; want %v", tc.dirs, got, err, tc.want)
		}
	}
	if _, err := affectedPackages(packages, []string{"deleted"}); err == nil {
		t.Fatal("a changed directory that is not a package must not be guessed")
	}

	// The same closure comes from real `go list` metadata of the fixture.
	repo, base := setupGoFastPassRepo(t, "  - name: test\n    command: go test -count=1 ./...\n    timeout: 5m\n", nil)
	commitRepoFile(t, repo, "leaf/leaf.go", passingLeaf)
	var events []string
	report, err := validateFastPass(t, t.Context(), repo, base, &events)
	if err != nil {
		t.Fatal(err)
	}
	if pre := report.CheckOutcomes[0]; !slices.Equal(pre.Packages, affectedByLeaf) {
		t.Fatalf("selection: %v", pre.Packages)
	}
}

func TestParseGoTestAll(t *testing.T) {
	for _, tc := range []struct {
		command string
		ok      bool
		build   []string
	}{
		{"go test ./...", true, nil},
		{"go test -race ./...", true, []string{"-race"}},
		{"  go test -count=1 -tags=integration -v ./...  ", true, []string{"-tags=integration"}},
		{"go test ./... -race", true, []string{"-race"}},
		{"go test", false, nil},
		{"go test ./internal/...", false, nil},
		{"go test ./... ./cmd/...", false, nil},
		{"go test ./... ./...", false, nil},
		{"go test -run Foo ./...", false, nil},
		{"go test -tags= ./...", false, nil},
		{"go test -race=1 ./...", false, nil},
		{"go test -coverprofile=c.out ./...", false, nil},
		{"go test -exec=/bin/true ./...", false, nil},
		{"go vet ./...", false, nil},
		{"go test ./... && echo done", false, nil},
		{"go test ./...; true", false, nil},
		{"go test $(echo ./...)", false, nil},
		{"go test './...'", false, nil},
		{"go test ./... > out", false, nil},
		{"env go test ./...", false, nil},
		{"/usr/bin/go test ./...", false, nil},
		{"go test ./...\nrm -rf x", false, nil},
	} {
		got, ok := parseGoTestAll(tc.command)
		if ok != tc.ok || (ok && !slices.Equal(got.buildFlags, tc.build)) {
			t.Errorf("parseGoTestAll(%q) = %+v, %v", tc.command, got, ok)
		}
	}
}

func TestFastPassFits(t *testing.T) {
	many := func(n, size int) []string {
		packages := make([]string, n)
		for i := range packages {
			packages[i] = "example.com/m/" + strings.Repeat("p", size) + strconv.Itoa(i)
		}
		return packages
	}
	for _, tc := range []struct {
		packages []string
		fits     bool
	}{
		{many(1, 1), true},
		{many(maxFastPassPackages, 1), true},
		{many(maxFastPassPackages+1, 1), false},
		{many(100, 400), false},
	} {
		if got := fastPassFits([]string{"-race"}, tc.packages); got != tc.fits {
			t.Errorf("%d packages of %d bytes: fits = %v", len(tc.packages), len(tc.packages[0]), got)
		}
	}
}

func TestParseGoListRejectsUnsafeMetadata(t *testing.T) {
	const mod = `"Module":{"Path":"example.com/m","Dir":"/m","Main":true}`
	for name, data := range map[string]string{
		"option-like":   `{"ImportPath":"-exec=x","Dir":"/m/x",` + mod + `}`,
		"shell":         `{"ImportPath":"example.com/m/$(x)","Dir":"/m/x",` + mod + `}`,
		"other module":  `{"ImportPath":"example.com/other","Dir":"/m/x",` + mod + `}`,
		"outside root":  `{"ImportPath":"example.com/m/x","Dir":"/elsewhere",` + mod + `}`,
		"not main":      `{"ImportPath":"example.com/m/x","Dir":"/m/x","Module":{"Path":"example.com/m","Dir":"/m"}}`,
		"invalid":       `{"ImportPath":`,
		"empty":         ``,
		"two modules":   `{"ImportPath":"example.com/m","Dir":"/m",` + mod + `}{"ImportPath":"example.com/m/x","Dir":"/n/x","Module":{"Path":"example.com/m","Dir":"/n","Main":true}}`,
		"dot traversal": `{"ImportPath":"example.com/m/../x","Dir":"/m/x",` + mod + `}`,
	} {
		if _, err := parseGoList([]byte(data)); err == nil {
			t.Errorf("%s: metadata accepted", name)
		}
	}
}
