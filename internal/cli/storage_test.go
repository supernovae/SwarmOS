package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/supernovae/swarmos/internal/storageuse"
	"github.com/supernovae/swarmos/internal/tracker"
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
