package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
	"github.com/supernovae/swarmos/internal/sandbox"
	"github.com/supernovae/swarmos/internal/testutil"
)

func TestDefaultModeNeverRunsHostAgent(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, name := range []string{"claude", "opencode", "omp"} {
		cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{name: {Binary: os.Args[0]}}}
		r, err := NewConfiguredRunner(name, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Provenance("developer", false).Sandbox; got != "harness-tools-bwrap" {
			t.Fatalf("%s selected %q", name, got)
		}
	}
}

func TestOperatorCannotWeakenApprovedIsolation(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", contract.ExecutionTrustedLocal)
	cfg := &contract.SwarmConfig{Security: contract.SecurityConfig{RequireIsolation: true}, Agents: map[string]contract.AgentConfig{"claude": {Binary: os.Args[0]}}}
	if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
		t.Fatal("local override weakened repository isolation")
	}
}

func TestMockCannotBypassIsolation(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, required := range []bool{false, true} {
		cfg := &contract.SwarmConfig{Security: contract.SecurityConfig{RequireIsolation: required}, Agents: map[string]contract.AgentConfig{"mock": {Binary: "mock"}}}
		if _, err := NewConfiguredRunner("mock", cfg, true); err == nil {
			t.Fatal("host mock accepted in isolated mode")
		}
	}
}

func TestIsolatedRunnerRejectsHostCredentialsAndUnrestrictedNetwork(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, ac := range []contract.AgentConfig{
		{Binary: "claude", Authentication: "none", Env: []string{"MODEL_API_KEY"}},
		{Binary: "claude", Authentication: "none", ReviewNetwork: true},
		{Binary: "claude", Authentication: "none", ContainerRuntime: "./docker"},
	} {
		cfg := &contract.SwarmConfig{Execution: contract.ExecutionConfig{Backend: contract.BackendOCI}, Agents: map[string]contract.AgentConfig{"claude": ac}}
		if _, err := NewConfiguredRunner("claude", cfg, false); err == nil {
			t.Fatal("accepted unsafe isolated capabilities")
		}
	}
}

func TestNativeCodexRejectsPolicyOverrides(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for _, ac := range []contract.AgentConfig{
		{Binary: os.Args[0], Args: []string{"--yolo"}},
		{Binary: os.Args[0], Env: []string{"OPENAI_API_KEY"}},
		{Binary: os.Args[0], ReviewNetwork: true},
		{Binary: os.Args[0], Authentication: "none"},
		{Binary: os.Args[0], Sandbox: "container"},
	} {
		cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"codex": ac}}
		if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
			t.Fatalf("accepted native override %+v", ac)
		}
	}
}

func TestNativeIsolatedEffortAdmitsNoOtherArguments(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	for name, effort := range map[string]string{"codex": "high", "claude": "max"} {
		cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{name: {Binary: os.Args[0], Effort: effort}}}
		r, err := NewConfiguredRunner(name, cfg, false)
		if err != nil {
			t.Fatalf("%s rejected approved effort: %v", name, err)
		}
		if got := r.Provenance("developer", false).Effort; got != effort {
			t.Fatalf("%s provenance effort = %q", name, got)
		}
		for _, ac := range []contract.AgentConfig{
			{Binary: os.Args[0], Effort: effort, Args: []string{"--effort", "low"}},
			{Binary: os.Args[0], Effort: effort, Env: []string{"OPENAI_API_KEY"}},
		} {
			cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{name: ac}}
			if _, err := NewConfiguredRunner(name, cfg, false); err == nil || !strings.Contains(err.Error(), "remove args, env") {
				t.Fatalf("%s accepted native override %+v: %v", name, ac, err)
			}
		}
	}
}

func TestIsolatedBackendIsExplicit(t *testing.T) {
	t.Setenv("SWARM_EXECUTION_MODE", "")
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{"codex": {Binary: os.Args[0]}}}
	r, err := NewConfiguredRunner("codex", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Provenance("developer", false).Sandbox; got != "codex-native" {
		t.Fatalf("default selected %q", got)
	}
	cfg.Execution.Backend = contract.BackendOCI
	if _, err := NewConfiguredRunner("codex", cfg, false); err == nil {
		t.Fatal("OCI silently reused host authentication")
	}
	ac := cfg.Agents["codex"]
	ac.Authentication = "none"
	cfg.Agents["codex"] = ac
	r, err = NewConfiguredRunner("codex", cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Provenance("developer", false).Sandbox; got != "container" {
		t.Fatalf("explicit OCI selected %q", got)
	}
}

func TestReadinessRoleBoundary(t *testing.T) {
	projects := []sandbox.ReadinessProject{{Directory: ".", Ecosystem: "go", Source: "go.mod"}}
	cfg := &contract.SwarmConfig{Agents: map[string]contract.AgentConfig{
		"native-dev":     {Harness: "codex", Binary: os.Args[0], Roles: []string{"developer"}},
		"delegated-test": {Harness: "opencode", Binary: os.Args[0], Roles: []string{"tester"}},
	}}
	opts := RunnerOptions{LookupEnv: func(string) (string, bool) { return "", false }}
	for _, tc := range []struct{ name, role, boundary string }{
		{"native-dev", "developer", "codex-native"},
		{"delegated-test", "tester", "harness-tools-bwrap"},
	} {
		report, err := CheckConfiguredReadiness(t.Context(), projects, tc.name, tc.role, cfg, true, opts, sandbox.Dependencies{}, "inspection")
		if err != nil || len(report) == 0 || report[0].Boundary != tc.boundary || report[0].Status != "unverified" {
			t.Fatalf("role=%s rows=%+v err=%v", tc.role, report, err)
		}
	}
	if runtime.GOOS == "linux" {
		_, err := CheckConfiguredReadiness(t.Context(), projects, "native-dev", "tester", cfg, true, opts, sandbox.Dependencies{}, "")
		if err == nil || !strings.Contains(err.Error(), "not permitted") {
			t.Fatalf("role eligibility: %v", err)
		}
	}
}

func TestReadinessPinnedOCIRoleOverride(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("readiness supports the Linux OCI worker")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "global.json"), []byte(`{"sdk":{"version":"8.0.100"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	projects, err := sandbox.DiscoverReadiness(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &contract.SwarmConfig{
		Execution: contract.ExecutionConfig{Mode: contract.ExecutionIsolated, Backend: contract.BackendOCI, ContainerRuntime: "docker", ContainerImage: "fixture/base@sha256:" + strings.Repeat("e", 64)},
		Agents: map[string]contract.AgentConfig{
			"oci-dev":  {Harness: "codex", Binary: "codex", Authentication: "none", Roles: []string{"developer"}},
			"oci-test": {Harness: "opencode", Binary: os.Args[0], Authentication: "none", Roles: []string{"tester"}, ContainerRuntime: "podman", ContainerImage: "fixture/test:unpinned"},
		},
	}
	opts := RunnerOptions{LookupEnv: func(string) (string, bool) { return "", false }}
	dev, err := CheckConfiguredReadiness(t.Context(), projects, "oci-dev", "developer", cfg, true, opts, sandbox.Dependencies{}, "inspection")
	if err != nil || !strings.HasPrefix(dev[0].Boundary, "oci:docker/fixture/base@sha256:") {
		t.Fatalf("developer=%+v err=%v", dev, err)
	}
	tester, err := CheckConfiguredReadiness(t.Context(), projects, "oci-test", "tester", cfg, true, opts, sandbox.Dependencies{}, "")
	if err == nil || !strings.Contains(err.Error(), "pinned by sha256") || tester[1].Boundary != "oci:podman/fixture/test:unpinned" || tester[1].Status != "failed" {
		t.Fatalf("tester=%+v err=%v", tester, err)
	}

	tools := t.TempDir()
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	t.Setenv("SWARM_GITHUB_TOKEN", "readiness-coordinator-sentinel")
	const inspected = `[{"HostConfig":{"NetworkMode":"none","PidMode":"private","IpcMode":"private","CgroupnsMode":"private","ReadonlyRootfs":true,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges"],"Memory":2147483648,"MemorySwap":2147483648,"NanoCpus":2000000000,"PidsLimit":128,"Tmpfs":{"/workspace":"rw,nosuid,nodev,size=512m,mode=0755,uid=65534,gid=65534","/output":"rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534","/home/worker":"rw,nosuid,nodev,size=64m,mode=0700,uid=65534,gid=65534","/tmp":"rw,nosuid,nodev,size=64m,mode=1777,uid=0,gid=0"}},"Mounts":[]}]`
	for _, present := range []bool{true, false} {
		digest, result := "c", "printf '8.0.100\n'"
		if !present {
			digest, result = "d", "printf 'missing dotnet SDK' >&2; exit 127"
		}
		image := "fixture/test@sha256:" + strings.Repeat(digest, 64)
		entry := cfg.Agents["oci-test"]
		entry.ContainerImage = image
		cfg.Agents["oci-test"] = entry
		calls := filepath.Join(t.TempDir(), "calls")
		script := "#!/bin/sh\n" +
			"test -z \"$SWARM_GITHUB_TOKEN\" || exit 91\n" +
			"printf '%s\\n' \"$*\" >> '" + calls + "'\n" +
			"case $1 in\n" +
			"image) printf '%s' '[{\"Os\":\"linux\",\"Config\":{}}]';;\n" +
			"inspect) printf '%s' '" + inspected + "';;\n" +
			"create) printf '%s\\n' '" + strings.Repeat("a", 64) + "';;\n" +
			"cp) cat >/dev/null;;\n" +
			"exec) case \"$*\" in *'dotnet --version'*) " + result + ";; esac;;\n" +
			"esac\n"
		if err := testutil.WriteExecutable(filepath.Join(tools, "podman"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		deps := sandbox.Dependencies{Caches: []sandbox.DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: "/host-cache-sentinel"}}}
		rows, err := CheckConfiguredReadiness(t.Context(), projects, "oci-test", "tester", cfg, true, opts, deps, "")
		if (err == nil) != present || rows[1].Boundary != "oci:podman/"+image || (!present && !strings.Contains(err.Error(), "missing dotnet SDK")) {
			t.Fatalf("present=%t rows=%+v err=%v", present, rows, err)
		}
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Fatal(err)
		}
		log := string(data)
		for _, want := range []string{"image inspect " + image, "--pull=never", "--network=none", "/bin/sh -c", "rm --force"} {
			if !strings.Contains(log, want) {
				t.Fatalf("missing %q in %s", want, log)
			}
		}
		for _, unwanted := range []string{"host-cache-sentinel", "readiness-coordinator-sentinel", "--mount", "pause "} {
			if strings.Contains(log, unwanted) {
				t.Fatalf("unexpected %q in %s", unwanted, log)
			}
		}
	}
}
