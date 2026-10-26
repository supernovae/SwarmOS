package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

func TestNodeProfileSaveLoadAndCapabilities(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "node.yaml")
	prof := DetectHostProfile("worker-node-1")
	prof.Capabilities = []string{"coding", "testing"}

	if err := SaveProfile(cfgPath, prof); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}

	loaded, err := LoadOrDetectProfile(cfgPath)
	if err != nil {
		t.Fatalf("LoadOrDetectProfile failed: %v", err)
	}
	if loaded.NodeID != "worker-node-1" || loaded.Resources.CPUs <= 0 {
		t.Fatalf("unexpected loaded profile: %+v", loaded)
	}

	if !MatchesCapabilities(loaded, []string{"coding", "testing"}) {
		t.Fatal("expected MatchesCapabilities to return true for coding+testing")
	}
	if MatchesCapabilities(loaded, []string{"coding", "gpu-training"}) {
		t.Fatal("expected MatchesCapabilities to return false for missing gpu-training")
	}
	if !SupportsPhaseCapability(loaded, "implement") {
		t.Fatal("expected node with coding capability to support implement phase")
	}
	if SupportsPhaseCapability(loaded, "review") {
		t.Fatal("expected node without review capability to reject review phase")
	}
}

func TestNodeProfileUpstreamRemote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	if got, err := LoadUpstreamRemote(path); err != nil || got != "" {
		t.Fatalf("missing profile = %q, %v", got, err)
	}
	prof := &contract.NodeProfile{NodeID: "n1", UpstreamRemote: "upstream"}
	if err := SaveProfile(path, prof); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadUpstreamRemote(path); err != nil || got != "upstream" {
		t.Fatalf("saved profile = %q, %v", got, err)
	}
	loaded, err := LoadOrDetectProfile(path)
	if err != nil || loaded.UpstreamRemote != "upstream" {
		t.Fatalf("round trip = %+v, %v", loaded, err)
	}
	raw, err := json.Marshal(loaded)
	if err != nil || !strings.Contains(string(raw), `"upstream_remote":"upstream"`) {
		t.Fatalf("json = %s, %v", raw, err)
	}
	if err := SaveProfile(path, &contract.NodeProfile{NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "upstream_remote") {
		t.Fatalf("unset upstream_remote written: %s", data)
	}
	if err := os.WriteFile(path, []byte("upstream_remote: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUpstreamRemote(path); err == nil || !strings.Contains(err.Error(), "parse node profile") {
		t.Fatalf("malformed profile = %v", err)
	}
}
