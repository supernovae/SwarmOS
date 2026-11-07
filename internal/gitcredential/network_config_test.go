package gitcredential

import "testing"

func TestIsNetworkWithConfiguration(t *testing.T) {
	for _, verb := range []string{"push", "fetch", "ls-remote"} {
		if !IsNetwork([]string{"-c", "core.hooksPath=/trusted/hooks", verb, "origin"}) {
			t.Errorf("configured %s escaped bounded network handling", verb)
		}
	}
	for _, args := range [][]string{{"-c"}, {"-c", "key=value"}, {"-c", "key=value", "status"}} {
		if IsNetwork(args) {
			t.Errorf("non-network command classified as network: %v", args)
		}
	}
}
