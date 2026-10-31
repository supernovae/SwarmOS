package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
