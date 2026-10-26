package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

func recognizedIDs(script string) []string {
	var ids []string
	for _, use := range recognizeTools(script) {
		ids = append(ids, use.tool.id)
	}
	return ids
}

func TestRecognizeTools(t *testing.T) {
	tests := []struct {
		script string
		want   []string
	}{
		{"gofmt -l .", []string{"gofmt"}},
		{`test -z "$(gofmt -s -l .)"`, []string{"gofmt"}},
		{"unformatted=$(gofmt -l .) || exit 1; if [ -n \"$unformatted\" ]; then exit 1; fi", []string{"gofmt"}},
		{"go vet ./...", []string{"go vet"}},
		{"go build ./... && go vet -tags e2e ./...", []string{"go vet"}},
		{"staticcheck ./...", []string{"staticcheck"}},
		{"go tool staticcheck ./...", []string{"staticcheck"}},
		{"go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...", []string{"staticcheck"}},
		{"$(go env GOPATH)/bin/staticcheck ./...", []string{"staticcheck"}},
		{"golangci-lint run --timeout 5m", []string{"golangci-lint"}},
		{"./node_modules/.bin/eslint src", []string{"eslint"}},
		{"npx --yes eslint .", []string{"eslint"}},
		{"pnpm exec eslint .", []string{"eslint"}},
		{"npx prettier --check .", []string{"prettier --check"}},
		{"prettier -c src", []string{"prettier --check"}},
		{"uv run ruff check .", []string{"ruff"}},
		{"python -m black --check .", []string{"black --check"}},
		{"cargo fmt --all -- --check", []string{"cargo fmt --check"}},
		{"cargo +nightly clippy --all-targets -- -D warnings", []string{"cargo clippy"}},
		{"CGO_ENABLED=0 env GOFLAGS=-mod=mod go vet ./...", []string{"go vet"}},
		{"set -e\ncd app \\\n  && npm ci\nnpx eslint . | tee lint.log", []string{"eslint"}},
		{"(cd sub && cargo clippy)", []string{"cargo clippy"}},
		{"go vet ./... && go vet ./cmd/...", []string{"go vet", "go vet"}},
		{"gofmt -l ${{ inputs.path }}", []string{"gofmt"}},
		// Unrecognized, mutating, or non-command mentions.
		{"go test ./...", nil},
		{"make lint", nil},
		{"${{ matrix.linter }} ./...", nil},
		{"# gofmt -l .\ngo build ./...", nil},
		{"echo 'run gofmt -l . locally'", nil},
		{`echo "staticcheck found nothing"`, nil},
		{"gofmtx -l .", nil},
		{"my-eslint-wrapper .", nil},
		{"go vetx ./...", nil},
		{"prettier --write .", nil},
		{"black .", nil},
		{"cargo fmt --all", nil},
		{"cargo build", nil},
		{"ruffle", nil},
	}
	for _, tt := range tests {
		if got := recognizedIDs(tt.script); !slices.Equal(got, tt.want) {
			t.Errorf("recognizeTools(%q) = %q; want %q", tt.script, got, tt.want)
		}
	}
}

func writeDriftRepo(t *testing.T, checks string, workflows map[string]string) string {
	t.Helper()
	root := t.TempDir()
	policy := "required_checks:\n" + checks + "protected_paths:\n  - .swarm/validation/**\n"
	files := map[string]string{".swarm/validation/required-checks.yaml": policy}
	for name, body := range workflows {
		files[".github/workflows/"+name] = body
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runWorkflow(commands ...string) string {
	var b strings.Builder
	b.WriteString("name: ci\non: [push]\njobs:\n  lint:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n")
	for _, command := range commands {
		b.WriteString("      - run: |\n")
		for _, line := range strings.Split(command, "\n") {
			b.WriteString("          " + line + "\n")
		}
	}
	return b.String()
}

func driftFor(t *testing.T, root string) []DoctorCheck {
	t.Helper()
	spec, err := LoadValidationSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	checks := doctorRequiredCheckDrift(root, spec)
	for _, check := range checks {
		if check.Status == "FAIL" || check.Category != "harness" || check.Name != ciDriftCheckName {
			t.Fatalf("drift check must be an advisory harness check: %+v", check)
		}
	}
	return checks
}

func TestDoctorDriftEachRecognizedTool(t *testing.T) {
	tools := []struct{ ci, check string }{
		{"gofmt -l .", GoFmtCheckCommand},
		{"go vet ./...", "go vet ./..."},
		{"go tool staticcheck ./...", "go tool staticcheck ./..."},
		{"golangci-lint run", "golangci-lint run ./..."},
		{"npx eslint .", "npm exec -- eslint src"},
		{"npx prettier --check .", "prettier -c ."},
		{"ruff check .", "ruff format --check ."},
		{"black --check .", "python -m black --check src"},
		{"cargo fmt --all -- --check", "cargo fmt --check"},
		{"cargo clippy -- -D warnings", "cargo clippy --all-targets"},
	}
	for _, tool := range tools {
		t.Run(tool.ci, func(t *testing.T) {
			workflows := map[string]string{"ci.yml": runWorkflow("go build ./...", tool.ci)}
			uncovered := driftFor(t, writeDriftRepo(t, "  - name: build\n    command: go build ./...\n    timeout: 5m\n", workflows))
			if len(uncovered) != 1 || uncovered[0].Status != "WARN" {
				t.Fatalf("uncovered %q: %+v", tool.ci, uncovered)
			}
			detail := uncovered[0].Detail
			if !strings.Contains(detail, ".github/workflows/ci.yml") || !strings.Contains(detail, tool.ci) {
				t.Fatalf("warning does not name workflow and command: %s", detail)
			}
			_, suggestion, ok := strings.Cut(detail, "entry: ")
			if !ok {
				t.Fatalf("warning lacks a suggestion: %s", detail)
			}
			var entries []contract.CheckSpec
			if err := yaml.Unmarshal([]byte(suggestion), &entries); err != nil || len(entries) != 1 {
				t.Fatalf("suggestion %q is not a YAML check entry: %v", suggestion, err)
			}
			policy := "required_checks:\n  - name: build\n    command: go build ./...\n    timeout: 5m\n  " + suggestion + "\nprotected_paths:\n  - .swarm/validation/**\n"
			spec, err := DecodeValidationSpec([]byte(policy))
			if err != nil {
				t.Fatalf("suggested entry is not a valid policy addition: %v\n%s", err, policy)
			}
			if got := recognizedIDs(spec.RequiredChecks[1].Command); len(got) != 1 || got[0] != recognizedIDs(tool.ci)[0] {
				t.Fatalf("suggested command %q does not cover %q", spec.RequiredChecks[1].Command, tool.ci)
			}

			covered := driftFor(t, writeDriftRepo(t, "  - name: lint\n    command: '"+strings.ReplaceAll(tool.check, "'", "''")+"'\n    timeout: 5m\n", workflows))
			if len(covered) != 1 || covered[0].Status != "OK" {
				t.Fatalf("covered %q by %q: %+v", tool.ci, tool.check, covered)
			}
		})
	}
}

func TestDoctorDriftReportsAndDeduplicatesAcrossWorkflows(t *testing.T) {
	root := writeDriftRepo(t, "  - name: lint\n    command: go vet ./...\n    timeout: 5m\n", map[string]string{
		"b.yaml":    runWorkflow("go vet ./...", "go tool staticcheck ./...\ngo tool staticcheck ./cmd/..."),
		"a.yml":     runWorkflow("test -z \"$(gofmt -l .)\"", "make lint", "echo staticcheck"),
		"notes.txt": "jobs: {x: {steps: [{run: gofmt -l .}]}}",
		"uses.yml":  "jobs:\n  release:\n    steps:\n      - uses: goreleaser/goreleaser-action@v6\n        with:\n          args: gofmt\n",
		"expr.yml":  runWorkflow("${{ matrix.command }}"),
	})
	checks := driftFor(t, root)
	if len(checks) != 2 {
		t.Fatalf("drift = %+v", checks)
	}
	if !strings.Contains(checks[0].Detail, ".github/workflows/a.yml") || !strings.Contains(checks[0].Detail, "(gofmt)") {
		t.Fatalf("first warning: %s", checks[0].Detail)
	}
	if !strings.Contains(checks[1].Detail, ".github/workflows/b.yaml") || !strings.Contains(checks[1].Detail, "go tool staticcheck ./...") || !strings.Contains(checks[1].Detail, "command: go tool staticcheck ./...") {
		t.Fatalf("second warning: %s", checks[1].Detail)
	}
}

func TestDoctorDriftSuggestionAvoidsExistingNames(t *testing.T) {
	root := writeDriftRepo(t, "  - name: fmt\n    command: npm run format\n    timeout: 5m\n", map[string]string{"ci.yml": runWorkflow("gofmt -l .")})
	checks := driftFor(t, root)
	if len(checks) != 1 || !strings.Contains(checks[0].Detail, "name: fmt-2") {
		t.Fatalf("suggestion reused an existing check name: %+v", checks)
	}
}

func TestDoctorDriftWorkflowErrorsAreAdvisory(t *testing.T) {
	checks := "  - name: build\n    command: go build ./...\n    timeout: 5m\n"
	if got := driftFor(t, writeDriftRepo(t, checks, nil)); len(got) != 1 || got[0].Status != "OK" || !strings.Contains(got[0].Detail, "no .github/workflows") {
		t.Fatalf("missing workflows: %+v", got)
	}
	malformed := driftFor(t, writeDriftRepo(t, checks, map[string]string{"bad.yml": "jobs: [unterminated\n", "ok.yml": runWorkflow("go vet ./...")}))
	if len(malformed) != 2 || !strings.Contains(malformed[0].Detail, "cannot parse .github/workflows/bad.yml") || !strings.Contains(malformed[1].Detail, "go vet") {
		t.Fatalf("malformed workflow: %+v", malformed)
	}
	if runtime.GOOS != "windows" {
		root := writeDriftRepo(t, checks, map[string]string{"ci.yml": runWorkflow("go vet ./...")})
		outside := filepath.Join(t.TempDir(), "outside.yml")
		if err := os.WriteFile(outside, []byte(runWorkflow("gofmt -l .")), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".github", "workflows", "linked.yml")); err != nil {
			t.Fatal(err)
		}
		got := driftFor(t, root)
		if len(got) != 1 || strings.Contains(got[0].Detail, "gofmt") {
			t.Fatalf("symlinked workflow was followed: %+v", got)
		}
		unreadable := filepath.Join(root, ".github", "workflows", "secret.yml")
		if err := os.WriteFile(unreadable, []byte(runWorkflow("gofmt -l .")), 0o000); err != nil {
			t.Fatal(err)
		}
		if os.Getuid() != 0 {
			got := driftFor(t, root)
			if len(got) != 2 || !strings.Contains(got[1].Detail, "cannot read .github/workflows/secret.yml") {
				t.Fatalf("unreadable workflow: %+v", got)
			}
		}
	}
}

func TestDoctorDriftNeverEditsPolicyOrWorkflows(t *testing.T) {
	root := writeDriftRepo(t, "  - name: build\n    command: go build ./...\n    timeout: 5m\n", map[string]string{"ci.yml": runWorkflow("gofmt -l .", "go vet ./...")})
	snapshot := func() map[string]string {
		files := map[string]string{}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			files[path] = string(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	before := snapshot()
	checks := RunDoctor(root, false)
	var warnings int
	for _, check := range checks {
		if check.Name == ciDriftCheckName {
			if check.Status != "WARN" {
				t.Fatalf("drift check status %+v", check)
			}
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("RunDoctor drift warnings = %d; checks %+v", warnings, checks)
	}
	after := snapshot()
	if len(before) != len(after) {
		t.Fatalf("doctor changed repository files: %v -> %v", before, after)
	}
	for path, data := range before {
		if after[path] != data {
			t.Fatalf("doctor edited %s", path)
		}
	}
}

func TestDoctorSkipsDriftForInvalidPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte(runWorkflow("gofmt -l .")), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, check := range RunDoctor(root, false) {
		if check.Name == ciDriftCheckName {
			t.Fatalf("drift ran without a valid policy: %+v", check)
		}
		if check.Name == ".swarm/validation/required-checks.yaml" && check.Status != "FAIL" {
			t.Fatalf("missing policy no longer fails doctor: %+v", check)
		}
	}
}
