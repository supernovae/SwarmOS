package harness

import (
	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/testutil/gitfixture"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimeoutPolicyIgnoresCandidateConfig(t *testing.T) {
	root := t.TempDir()
	if _, err := InitRepository(InitOptions{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := gitfixture.Command(t, "", append([]string{"-C", root}, args...)...)
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s %v", args, data, err)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.test")
	config := filepath.Join(root, ".swarm", "config.yaml")
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	approved := strings.Replace(string(original), "execution:", "execution:\n  agent_timeout: 2h", 1)
	if err := os.WriteFile(config, []byte(approved), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "approved")
	base := git("rev-parse", "HEAD")
	for _, candidate := range []string{"1ms", "24h"} {
		data := strings.Replace(approved, "agent_timeout: 2h", "agent_timeout: "+candidate, 1)
		if err := os.WriteFile(config, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigAt(root, base)
		if err != nil {
			t.Fatal(err)
		}
		limits, err := contract.ResolveAgentLimits(cfg.Execution, "developer", false, 0)
		if err != nil || limits.Wall != 2*time.Hour {
			t.Fatalf("%+v %v", limits, err)
		}
	}
}
