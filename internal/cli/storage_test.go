package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, path := range []string{filepath.Dir(registry), marker} {
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
