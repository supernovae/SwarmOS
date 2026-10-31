package node

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

func TestStorageSettingsDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.yaml")
	got, e := LoadStorageSettings(path)
	if e != nil || got != DefaultStorageSettings() {
		t.Fatal(got, e)
	}
	for _, body := range []string{"cache_target_bytes: 0", "cache_target_bytes: -1", "cache_target_bytes: 9223372036854775808", "inactive_retention: 0h", "inactive_retention: 999999999999999h", "maintenance_interval: nonsense", "warning_interval: -1m", "low_free_bytes: 0", "low_free_percent: 0", "low_free_percent: 101", "low_free_percent: .nan", "surprise: true", "low_free_percent: nope"} {
		t.Run(body, func(t *testing.T) {
			if e := os.WriteFile(path, []byte("storage:\n  "+body+"\n"), 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := LoadStorageSettings(path); e == nil {
				t.Fatal("accepted invalid storage settings")
			}
		})
	}
	data := []byte("node_id: test\nstorage:\n  cache_target_bytes: 123\n  inactive_retention: 1h\n  low_free_bytes: 456\n  low_free_percent: 7.5\n  maintenance_interval: 2m\n  warning_interval: 1m\n")
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	got, e = LoadStorageSettings(path)
	if e != nil || got.CacheTargetBytes != 123 || got.InactiveRetention != time.Hour || got.LowFreePercent != 7.5 {
		t.Fatal(got, e)
	}
	var prof contract.NodeProfile
	if e = yaml.Unmarshal(data, &prof); e != nil {
		t.Fatal(e)
	}
	raw, e := yaml.Marshal(prof)
	if e != nil {
		t.Fatal(e)
	}
	var round contract.NodeProfile
	if e = yaml.Unmarshal(raw, &round); e != nil || !reflect.DeepEqual(prof.Storage, round.Storage) {
		t.Fatal(e, round)
	}
}
func TestStorageCandidateConfigIgnored(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SWARM_CACHE_TARGET_BYTES", "1")
	t.Setenv("SWARM_STORAGE_INACTIVE_RETENTION", "1ns")
	candidate := filepath.Join(root, ".swarm")
	if e := os.Mkdir(candidate, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(candidate, "config.yaml"), []byte("storage:\n  cache_target_bytes: 1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	profile := filepath.Join(root, "node.yaml")
	if e := os.WriteFile(profile, []byte("storage:\n  cache_target_bytes: 999\n"), 0600); e != nil {
		t.Fatal(e)
	}
	got, e := LoadStorageSettings(profile)
	if e != nil || got.CacheTargetBytes != 999 || got.InactiveRetention != 14*24*time.Hour {
		t.Fatal(got, e)
	}
}

func TestStorageSettingsInvalidDoesNotBlockExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.yaml")
	if e := os.WriteFile(path, []byte("node_id: worker\nupstream_remote: upstream\nstorage:\n  cache_target_bytes: invalid\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadStorageSettings(path); e == nil {
		t.Fatal("invalid configuration accepted")
	}
	if prof, e := LoadOrDetectProfile(path); e != nil || prof.NodeID != "worker" {
		t.Fatal(prof, e)
	}
	if remote, e := LoadUpstreamRemote(path); e != nil || remote != "upstream" {
		t.Fatal(remote, e)
	}
	if _, e := DependencyCacheMode(path); e != nil {
		t.Fatal(e)
	}
}

func TestStorageCandidateRelativeProfileRejected(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("SWARM_NODE_CONFIG", "")
	t.Chdir(t.TempDir())
	if e := os.Mkdir(".swarm", 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(".swarm", "node.yaml"), []byte("storage:\n  cache_target_bytes: 1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if got, e := LoadStorageSettings(""); e == nil || got != DefaultStorageSettings() {
		t.Fatal("candidate fallback profile admitted", got, e)
	}
}
