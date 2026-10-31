package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/coordination"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/storage"
)

func TestStorageReportOutsideCheckoutNoRegistryMutation(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	scratch := filepath.Join(root, "scratch")
	registry := filepath.Join(root, "missing", "repos.yaml")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("TMPDIR", scratch)
	// A token command must not execute even when a selected checkout is scanned.
	marker := filepath.Join(root, "credential-command-ran")
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "touch "+marker)
	for _, jsonOutput := range []bool{false, true} {
		opts := &RootOptions{lookupEnv: func(key string) (string, bool) {
			if key == "SWARM_REGISTRY_PATH" {
				return registry, true
			}
			return "", false
		}}
		cmd := newRootCmdWithOptions(opts)
		var output bytes.Buffer
		cmd.SetOut(&output)
		args := []string{"storage", "report", "--repo", "missing/repo"}
		if jsonOutput {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var snap storage.Snapshot
			if err := json.Unmarshal(output.Bytes(), &snap); err != nil || snap.Complete {
				t.Fatalf("%s %v", output.String(), err)
			}
		} else if !strings.Contains(output.String(), "complete=false") {
			t.Fatal(output.String())
		}
	}
	for _, path := range []string{filepath.Dir(registry), marker, filepath.Join(cache, "swarm-use")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("created %s: %v", path, err)
		}
	}
}
func TestStorageReportRejectsForkAndApply(t *testing.T) {
	for _, args := range [][]string{{"storage", "report", "--fork"}, {"storage", "report", "--apply"}, {"storage", "report", "--fork-remote", "fork"}} {
		cmd := newRootCmdWithOptions(&RootOptions{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("accepted", args)
		}
	}
}
func TestStorageRenderersShareUnknownFields(t *testing.T) {
	s := storage.Snapshot{Version: 1, Complete: false, Scope: "fixture", Resources: []storage.Resource{{Owner: "unknown", Class: "review-scratch", Path: "/fixture", Eligibility: "unknown", Reason: "unproven"}}}
	var human, encoded bytes.Buffer
	if err := renderStorageReport(&human, s, false); err != nil {
		t.Fatal(err)
	}
	if err := renderStorageReport(&encoded, s, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"review-scratch", "unknown", "/fixture", "unproven"} {
		if !strings.Contains(human.String(), want) || !strings.Contains(encoded.String(), want) {
			t.Fatal(want)
		}
	}
	if !strings.Contains(encoded.String(), `"logical_bytes": null`) {
		t.Fatal(encoded.String())
	}
}

func TestStorageBothRenderersPreserveAuthoritativeState(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".swarm", "coordination", "issue-1.json"), `{"lease":{"version":1,"issue_number":1,"status":"active","claim_id":"private-token"},"pr_number":2,"artifacts":[{"path":"private-artifact"}]}`)
	write(filepath.Join(repo, ".swarm", "local-tracker", "state.json"), `{"pull_requests":{"2":{"number":2,"state":"open"}}}`)
	write(filepath.Join(root, "cache", "swarm", "agent-build", "orphan", "go", "file"), "cache")
	write(filepath.Join(root, "scratch", "swarm-review-fixture", "file"), "scratch")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("TMPDIR", filepath.Join(root, "scratch"))
	type meta struct {
		mode  os.FileMode
		mtime time.Time
		data  string
	}
	snapshot := func() map[string]meta {
		t.Helper()
		out := map[string]meta{}
		err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			m := meta{mode: info.Mode(), mtime: info.ModTime()}
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				m.data = string(data)
			}
			out[path] = m
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	for _, cancelled := range []bool{false, true} {
		for _, jsonOutput := range []bool{false, true} {
			options := &RootOptions{RepoRoot: repo, Backend: "local", lookupEnv: func(key string) (string, bool) {
				if key == "SWARM_REGISTRY_PATH" {
					return filepath.Join(root, "absent", "repos.yaml"), true
				}
				return "", false
			}}
			cmd := newRootCmdWithOptions(options)
			var output bytes.Buffer
			cmd.SetOut(&output)
			args := []string{"storage", "report"}
			if jsonOutput {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			ctx, cancel := context.WithCancel(context.Background())
			if cancelled {
				cancel()
			}
			err := cmd.ExecuteContext(ctx)
			cancel()
			if cancelled && !errors.Is(err, context.Canceled) || !cancelled && err != nil {
				t.Fatal(err)
			}
			if jsonOutput {
				var out storage.Snapshot
				if err := json.Unmarshal(output.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if cancelled && out.Complete {
					t.Fatal("cancelled snapshot complete")
				}
			}
			for _, secret := range []string{"private-token", "private-artifact"} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("private state exposed")
				}
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("report changed filesystem or authoritative state")
			}
		}
	}
}

func TestStorageCleanupPreviewApplyAndJSON(t *testing.T) {
	f := newCLIFixture(t)
	task := bulkMerged(t, f)
	t.Setenv("TMPDIR", t.TempDir())
	// Keep the fixture within the existing inventory string budget. Full mock
	// agent transcripts can exceed it and intentionally produce unknown evidence.
	trackerPath := filepath.Join(f.root, ".swarm", "local-tracker", "state.json")
	raw, e := os.ReadFile(trackerPath)
	if e != nil {
		t.Fatal(e)
	}
	var data any
	if e = json.Unmarshal(raw, &data); e != nil {
		t.Fatal(e)
	}
	var compact func(any)
	compact = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if k == "body" {
					x[k] = ""
				} else {
					compact(item)
				}
			}
		case []any:
			for _, item := range x {
				compact(item)
			}
		}
	}
	compact(data)
	raw, e = json.Marshal(data)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(trackerPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	// Observe the complete checkout, including modes and timestamps. Preview may
	// read existing objects but must not initialize locks, registries or ledgers.
	snapshot := func() map[string]string {
		out := map[string]string{}
		err := filepath.WalkDir(f.root, func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			value := info.Mode().String() + info.ModTime().String()
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += string(data)
			}
			out[path] = value
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	marker := filepath.Join(t.TempDir(), "credential-command-ran")
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "touch "+marker)
	stdout, _, err := runSplit(f, "storage", "cleanup", "--json")
	if err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("preview executed credential command", e)
	}
	t.Setenv("SWARM_GITHUB_TOKEN_COMMAND", "")
	var report storageCleanupReport
	if err = json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err, stdout)
	}
	if report.Version != 1 || report.Mode != "preview" || len(report.Tasks) != 1 || report.Totals.Eligible == 0 {
		t.Fatalf("%+v", report)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		for path, value := range after {
			if before[path] != value {
				t.Errorf("preview changed %s", path)
			}
		}
		t.Fatal("preview changed checkout")
	}
	stdout, _, err = runSplit(f, "storage", "cleanup", "--apply", "--json")
	if err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	if err = json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Mode != "apply" || report.Totals.Retired == 0 {
		t.Fatalf("%+v", report)
	}
	if _, err = os.Stat(filepath.Join(f.root, ".swarm", "worktrees", fmt.Sprintf("issue-%d", task.issue))); !os.IsNotExist(err) {
		t.Fatal("worktree retained", err)
	}
	stdout, _, err = runSplit(f, "storage", "cleanup", "--apply", "--json")
	if err != nil {
		t.Fatalf("rerun %v: %s", err, stdout)
	}
}
func TestStorageCleanupSelectorsAndErrors(t *testing.T) {
	for _, args := range [][]string{{"--all-repos", "--root", "/missing"}, {"--all-repos", "--repo", "owner/repo"}, {"unexpected"}, {"--root", "/missing", "--backend", "local"}} {
		cmd := newRootCmdWithOptions(&RootOptions{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"storage", "cleanup", "--json"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("accepted", args)
		}
		var report storageCleanupReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Errors) == 0 {
			t.Fatalf("missing single error report: %s %v", stdout.String(), err)
		}
	}
}

type storageDeniedTracker struct{ *tracker.LocalTracker }

func (*storageDeniedTracker) Authorize(context.Context, string) error {
	return errors.New("operator permission denied")
}
func TestStorageCleanupAuthorizationProtectsHost(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	common := filepath.Join(f.root, ".git")
	hash := sha256.Sum256([]byte(common))
	key := hex.EncodeToString(hash[:16])
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "swarm")
	control, _ := storageuse.ControlRoot()
	manager, err := storageuse.Open(control, cache, os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	path := filepath.Join(cache, "agent-build", key, "go")
	use, err := manager.AcquireUse(context.Background(), storageuse.ResourceFor(key, "agent-build", path))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "data"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(&RootOptions{trackerOverride: &storageDeniedTracker{f.tracker}})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--root", f.root, "--backend", "local", "storage", "cleanup", "--apply", "--json"})
	if err = cmd.Execute(); err == nil {
		t.Fatal("unauthorized apply succeeded")
	}
	var report storageCleanupReport
	if err = json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err, stdout.String())
	}
	if len(report.Tasks) != 0 || len(report.Errors) == 0 {
		t.Fatalf("%+v", report)
	}
	if raw, err := os.ReadFile(filepath.Join(path, "data")); err != nil || string(raw) != "keep" {
		t.Fatal("unauthorized host deletion", err)
	}
}
func TestStorageCleanupAllReposContinuesAfterInvalidScope(t *testing.T) {
	f := newCLIFixture(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	registryPath := filepath.Join(t.TempDir(), "repos.yaml")
	f.setenv("SWARM_REGISTRY_PATH", registryPath)
	raw := fmt.Sprintf("version: 1\nrepos:\n  a/missing:\n    repo: a/missing\n    path: %s\n    backend: local\n  b/valid:\n    repo: b/valid\n    path: %s\n    backend: local\n", filepath.Join(t.TempDir(), "gone"), f.root)
	if err := os.WriteFile(registryPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(nil)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"storage", "cleanup", "--all-repos", "--apply", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("invalid scope silently ignored")
	}
	var report storageCleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err, stdout.String())
	}
	if len(report.Incomplete) == 0 || len(report.Errors) == 0 || len(report.Tasks) != 1 || report.Tasks[0].Status != "completed" {
		t.Fatalf("%+v", report)
	}
}

// The fixture writes each ledger through a file, never one dataset-sized buffer.
func inventoryScaleObjects(t *testing.T, root string, count int, size func(int) int64) (map[int]string, []*tracker.PullRequest, int64, int64) {
	t.Helper()
	git := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	refs := map[int]string{}
	prs := []*tracker.PullRequest{}
	var total, largest int64
	chunk := strings.Repeat("x", 8192)
	for n := 1; n <= count; n++ {
		path := filepath.Join(t.TempDir(), "state.json")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		prefix := fmt.Sprintf(`{"lease":{"version":1,"issue_number":%d,"status":"completed","phase":"submitted"},"branch":"swarm/issue-%d-test","submitted_head_sha":"abc","pr_number":%d,"pr_url":"https://example.test/pr/%d","artifacts":[{"body":"`, n, n, 1000+n, 1000+n)
		suffix := `"}],"role_runs":[{"history":[true,null,{"ignored":"escaped\nvalue"}]}]}`
		want := size(n)
		padding := want - int64(len(prefix)+len(suffix))
		if padding < 0 {
			t.Fatal("invalid fixture size")
		}
		if _, err = io.WriteString(f, prefix); err != nil {
			t.Fatal(err)
		}
		for padding > 0 {
			part := int64(len(chunk))
			if padding < part {
				part = padding
			}
			if _, err = io.WriteString(f, chunk[:part]); err != nil {
				t.Fatal(err)
			}
			padding -= part
		}
		if _, err = io.WriteString(f, suffix); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		blob := git("", "hash-object", "-w", path)
		tree := git("100644 blob "+blob+"\tstate.json\n", "mktree")
		sha := git("", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit-tree", tree, "-m", "inventory")
		git("", "update-ref", fmt.Sprintf("refs/heads/swarm-state/issue-%d", n), sha)
		refs[n] = sha
		total += want
		if want > largest {
			largest = want
		}
		now := time.Now()
		prs = append(prs, &tracker.PullRequest{Number: 1000 + n, HTMLURL: fmt.Sprintf("https://example.test/pr/%d", 1000+n), State: "closed", Merged: true, MergedAt: &now, MergeCommitSHA: "def", HeadSHA: "abc", Head: fmt.Sprintf("swarm/issue-%d-test", n), HeadRepo: "fixture/repo", BaseRepo: "fixture/repo"})
	}
	return refs, prs, total, largest
}

type inventoryScaleVisitor struct {
	refs map[int]string
	prs  []*tracker.PullRequest
}

func (v inventoryScaleVisitor) UpstreamRepo() string { return "fixture/repo" }
func (v inventoryScaleVisitor) VisitStateRefs(ctx context.Context, b tracker.InventoryBudget, visit func(int, string) error) error {
	for n, sha := range v.refs {
		if err := visit(n, sha); err != nil {
			return err
		}
	}
	return nil
}
func (v inventoryScaleVisitor) VisitPullRequests(ctx context.Context, b tracker.InventoryBudget, visit func(*tracker.PullRequest) error) error {
	for _, pr := range v.prs {
		if err := visit(pr); err != nil {
			return err
		}
	}
	return nil
}

type storageScaleTracker struct {
	*tracker.LocalTracker
	inventoryScaleVisitor
}

func (t storageScaleTracker) UpstreamRepo() string { return "fixture/repo" }
func (t storageScaleTracker) VisitStateRefs(ctx context.Context, b tracker.InventoryBudget, v func(int, string) error) error {
	return t.inventoryScaleVisitor.VisitStateRefs(ctx, b, v)
}
func (t storageScaleTracker) VisitPullRequests(ctx context.Context, b tracker.InventoryBudget, v func(*tracker.PullRequest) error) error {
	return t.inventoryScaleVisitor.VisitPullRequests(ctx, b, v)
}

func storageScaleHost(t *testing.T, root string) string {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	return storageScaleResource(t, root)
}
func storageScaleResource(t *testing.T, root string) string {
	t.Helper()
	common := filepath.Join(root, ".git")
	hash := sha256.Sum256([]byte(common))
	key := hex.EncodeToString(hash[:16])
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "swarm")
	control, err := storageuse.ControlRoot()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := storageuse.Open(control, cache, os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	path := filepath.Join(cache, "agent-build", key, "go")
	use, err := manager.AcquireUse(context.Background(), storageuse.ResourceFor(key, "agent-build", path))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "data"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = use.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestStorageCleanupRealLedgerScale(t *testing.T) {
	f := newCLIFixture(t)
	prGit(t, f.root, "remote", "add", "origin", "https://github.com/fixture/repo.git")
	path := storageScaleHost(t, f.root)
	refs, prs, total, largest := inventoryScaleObjects(t, f.root, 166, func(n int) int64 {
		if n == 1 {
			return 4533441
		}
		if n == 166 {
			return 648470
		}
		return 648464
	})
	tr := storageScaleTracker{f.tracker, inventoryScaleVisitor{refs, prs}}
	cmd := f.command(&RootOptions{trackerOverride: tr})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--root", f.root, "--repo", "fixture/repo", "--backend", "github", "storage", "cleanup", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	var report storageCleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err, stdout.String())
	}
	if !report.Host.Complete || len(report.Host.Resources) != 1 || report.Host.Resources[0].Outcome != storage.ReclaimEligible || len(report.Tasks) != 1 {
		t.Fatalf("%+v", report)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	t.Logf("refs=%d total=%d largest=%d host resources=%d", len(refs), total, largest, len(report.Host.Resources))
}
func TestStorageCleanupInventoryFailureReport(t *testing.T) {
	f := newCLIFixture(t)
	path := storageScaleHost(t, f.root)
	refs, prs, _, _ := inventoryScaleObjects(t, f.root, 1, func(int) int64 { return coordination.InventoryInputLimit + 1 })
	if out, err := exec.Command("git", "-C", f.root, "remote", "add", "origin", "https://github.com/fixture/repo.git").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	tr := storageScaleTracker{f.tracker, inventoryScaleVisitor{refs, prs}}
	for _, apply := range []bool{false, true} {
		cmd := f.command(&RootOptions{trackerOverride: tr})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		args := []string{"--root", f.root, "--repo", "fixture/repo", "--backend", "github", "storage", "cleanup", "--json"}
		if apply {
			// Local apply consumes the same oversized pinned ledger through safefs.
			dir := filepath.Join(f.root, ".swarm", "coordination")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			out, err := os.Create(filepath.Join(dir, "issue-1.json"))
			if err != nil {
				t.Fatal(err)
			}
			show := exec.Command("git", "-C", f.root, "show", refs[1]+":state.json")
			show.Stdout = out
			if err = show.Run(); err != nil {
				t.Fatal(err)
			}
			if err = out.Close(); err != nil {
				t.Fatal(err)
			}
			for i := range args {
				if args[i] == "github" {
					args[i] = "local"
				}
			}
			args = append(args, "--apply")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("read failure returned success")
		}
		var report storageCleanupReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatal(err, stdout.String())
		}
		if report.Host.Complete || len(report.Host.Resources) != 1 || report.Host.Resources[0].Outcome != storage.ReclaimRetained || len(report.Errors) == 0 || strings.Contains(stdout.String(), "signal: killed") || !strings.Contains(stdout.String(), "limit") {
			t.Fatalf("%+v", report)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageCleanupInventoryFailureContinuesHealthyApply(t *testing.T) {
	f := newCLIFixture(t)
	bad := storageScaleHost(t, f.root)
	healthy := newCLIFixture(t)
	good := storageScaleResource(t, healthy.root)
	for _, root := range []string{f.root, healthy.root} {
		if err := os.MkdirAll(filepath.Join(root, ".swarm", "coordination"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, ".swarm", "coordination", "issue-1.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	body := `{"lease":{"version":1,"issue_number":1,"status":"completed"},"branch":"swarm/issue-1-test","submitted_head_sha":"abc","pr_number":1001}`
	if err := os.WriteFile(filepath.Join(healthy.root, ".swarm", "coordination", "issue-1.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, ".swarm", "worktrees", "issue-1"), 0700); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(t.TempDir(), "repos.yaml")
	f.setenv("SWARM_REGISTRY_PATH", registry)
	raw := fmt.Sprintf("version: 1\nrepos:\n  a/bad:\n    repo: a/bad\n    path: %s\n    backend: local\n  b/good:\n    repo: b/good\n    path: %s\n    backend: local\n", f.root, healthy.root)
	if err := os.WriteFile(registry, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pr := &tracker.PullRequest{Number: 1001, State: "closed", Merged: true, MergedAt: &now, MergeCommitSHA: "def", HeadSHA: "abc", Head: "swarm/issue-1-test"}
	tr := storageScaleTracker{f.tracker, inventoryScaleVisitor{prs: []*tracker.PullRequest{pr}}}
	cmd := f.command(&RootOptions{trackerOverride: tr})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"storage", "cleanup", "--all-repos", "--apply", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing read failure")
	}
	var report storageCleanupReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]storage.ReclamationOutcome{}
	for _, r := range report.Host.Resources {
		outcomes[r.Resource.Location] = r.Outcome
	}
	if report.Host.Complete || outcomes[bad] != storage.ReclaimRetained || outcomes[good] != storage.ReclaimRemoved || len(report.Tasks) != 2 || len(report.Tasks[0].Unresolved) != 1 {
		t.Fatalf("%+v", report)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
