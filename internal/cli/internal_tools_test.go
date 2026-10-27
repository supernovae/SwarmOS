package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestInternalToolsBuildCacheFlag(t *testing.T) {
	work := t.TempDir()
	caches, err := parseBuildCaches([]string{"go=/abs/go", "rust=/abs/rust", "c-cpp=/abs/cc"}, work)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GOCACHE": "/abs/go", "CARGO_TARGET_DIR": "/abs/rust", "CCACHE_DIR": "/abs/cc"}
	if len(caches) != len(want) {
		t.Fatalf("caches = %+v", caches)
	}
	for _, c := range caches {
		if want[c.EnvKey] != c.Dir || c.Overlay {
			t.Fatalf("cache %+v, want %s=%s", c, c.EnvKey, want[c.EnvKey])
		}
	}
	for _, value := range []string{
		"maven=/abs",                         // deferred ecosystem
		"GOCACHE=/abs",                       // env keys cannot be chosen by the caller
		"go=GOCACHE",                         // relative path
		"go=relative/cache",                  // relative path
		"go=" + filepath.Join(work, "cache"), // inside the workspace
		"go",                                 // missing directory
	} {
		if _, err := parseBuildCaches([]string{value}, work); err == nil {
			t.Errorf("accepted --build-cache %q", value)
		}
		cmd := newInternalToolsCmd()
		cmd.SetArgs([]string{"--workspace", work, "--build-cache", value})
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("internal-tools accepted --build-cache %q", value)
		}
	}
}
