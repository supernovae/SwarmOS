package node

import (
	"path/filepath"
	"testing"
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
