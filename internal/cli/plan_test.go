package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/tracker"
	"github.com/supernovae/swarmos/internal/workflow"
	"gopkg.in/yaml.v3"
)

func exportRoadmap(t *testing.T, f *cliFixture) string {
	t.Helper()
	path := filepath.Join(f.root, "ROADMAP.md")
	text := "# Release\n\n## First task\n### Objective\nBuild the first feature.\n### Acceptance Criteria\n- [ ] First works\n\n## Second task\n### Objective\nBuild the second feature.\n### Acceptance Criteria\n- [ ] Second works\ndepends on: 1\n"
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readPlanExport(t *testing.T, path string) ([]byte, *workflow.ProjectPlanReport) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r workflow.ProjectPlanReport
	if strings.EqualFold(filepath.Ext(path), ".json") {
		err = json.Unmarshal(data, &r)
	} else {
		err = yaml.Unmarshal(data, &r)
	}
	if err != nil {
		t.Fatal(err)
	}
	return data, &r
}

func testPlanExportMode(t *testing.T, scan bool) {
	f := newCLIFixture(t)
	args := []string{"repo", "plan", "--from", exportRoadmap(t, f)}
	mode := "from-plan"
	if scan {
		f.create(t, "agent")
		if _, err := f.tracker.CreateIssue(context.Background(), "Raw idea", "Please improve this", nil); err != nil {
			t.Fatal(err)
		}
		args = []string{"repo", "plan", "--scan"}
		mode = "backlog-scan"
	}
	before, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	stdout := f.must(t, args...)
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		path := filepath.Join(t.TempDir(), "snapshot"+ext)
		if err := os.WriteFile(path, []byte("old content to replace"), 0644); err != nil {
			t.Fatal(err)
		}
		command := append(append([]string{}, args...), "--export", path)
		if got := f.must(t, command...); got != stdout {
			t.Fatal("stdout changed")
		}
		data, r := readPlanExport(t, path)
		if r.Mode != mode || r.PolicyDigest == "" || r.GoalsDigest == "" || r.BaseSHA == "" || r.SummaryMarkdown != stdout || len(r.ExecutionWaves) == 0 {
			t.Fatalf("incomplete report: %#v", r)
		}
		if scan {
			if len(r.BacklogItems) != 2 || r.BacklogItems[0].IssueNumber == 0 || r.BacklogItems[0].RecommendedLabel == "" {
				t.Fatalf("backlog: %#v", r.BacklogItems)
			}
		} else if len(r.ProposedUnits) != 2 || r.ProposedUnits[0].Title != "First task" || !reflect.DeepEqual(r.ProposedUnits[1].DependsOn, []int{1}) {
			t.Fatalf("units: %#v", r.ProposedUnits)
		}
		f.must(t, command...)
		repeated, _ := readPlanExport(t, path)
		if !bytes.Equal(data, repeated) {
			t.Fatal("snapshot changed")
		}
	}
	after, err := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("preview changed tracker", err)
	}
}
func TestCLIPlanExportScan(t *testing.T) { testPlanExportMode(t, true) }
func TestCLIPlanExportFrom(t *testing.T) { testPlanExportMode(t, false) }

func TestCLIPlanExportJSONStdout(t *testing.T) {
	f := newCLIFixture(t)
	path := filepath.Join(t.TempDir(), "snapshot.yaml")
	out := f.must(t, "repo", "plan", "--scan", "--json", "--export", path)
	var envelope struct {
		Kind   string
		Report workflow.ProjectPlanReport
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	_, r := readPlanExport(t, path)
	if envelope.Kind != "project-plan" || !reflect.DeepEqual(&envelope.Report, r) {
		t.Fatal("envelope differs from snapshot")
	}
}

func TestCLIPlanExportValidation(t *testing.T) {
	f := newCLIFixture(t)
	for _, args := range [][]string{
		{"--scan", "--export", "snapshot.txt", "--apply"},
		{"--scan", "--export", "snapshot"},
		{"--scan", "--export="},
		{"--scan", "--from", "missing.md", "--export", "snapshot.json"},
		{"--export", "snapshot.json"},
	} {
		if _, err := f.run(append([]string{"repo", "plan"}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	f.must(t, "repo", "plan", "--scan", "--export", "relative.JSON")
	readPlanExport(t, filepath.Join(dir, "relative.JSON"))
}

func TestCLIPlanExportErrors(t *testing.T) {
	f := newCLIFixture(t)
	dir := t.TempDir()
	directory := filepath.Join(dir, "directory.yaml")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{directory, filepath.Join(dir, "missing", "snapshot.json")} {
		if _, err := f.run("repo", "plan", "--scan", "--export", path); err == nil || !strings.Contains(err.Error(), "export project plan") {
			t.Fatalf("write error: %v", err)
		}
	}
	path := filepath.Join(dir, "existing.json")
	original := []byte("keep me")
	os.WriteFile(path, original, 0644)
	for _, target := range []string{path, filepath.Join(dir, "new.json")} {
		if _, err := f.run("repo", "plan", "--from", "missing.md", "--export", target); err == nil {
			t.Fatal("expected planner error")
		}
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("destination changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.json")); !os.IsNotExist(err) {
		t.Fatal("created failed export")
	}
}

func TestCLIPlanExportApply(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "write-failure"}[fail], func(t *testing.T) {
			f := newCLIFixture(t)
			path := filepath.Join(t.TempDir(), "snapshot.json")
			if fail {
				path = filepath.Join(path, "missing.json")
			}
			_, err := f.run("repo", "plan", "--from", exportRoadmap(t, f), "--apply", "--export", path)
			if (err != nil) != fail {
				t.Fatalf("apply: %v", err)
			}
			issues, listErr := f.tracker.ListIssues(context.Background(), tracker.IssueFilter{State: "open"})
			if listErr != nil || len(issues) != 3 {
				t.Fatalf("apply issues: %d, %v", len(issues), listErr)
			}
			if !fail {
				_, r := readPlanExport(t, path)
				if !r.EpicCreated || r.EpicIssue == nil || len(r.CreatedIssues) != 2 {
					t.Fatalf("final report: %#v", r)
				}
			}
		})
	}
}
