package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func writeReadinessFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadinessMixedApprovedBase(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	writeReadinessFixture(t, root, map[string]string{
		"web/package.json":      `{"name":"web","version":"1.0.0"}`,
		"service/go.mod":        "module example.com/service\ngo 1.25\n",
		"cli/Cargo.toml":        "[package]\nname='fixture'\nversion='0.1.0'\n",
		"sdk/global.json":       `{"sdk":{"version":"8.0.100"}}`,
		"native/CMakeLists.txt": "project(fixture LANGUAGES C CXX)\n",
	})
	git("add", ".")
	git("-c", "user.name=Readiness", "-c", "user.email=readiness@example.com", "commit", "-qm", "approved")
	base := git("rev-parse", "HEAD")
	writeReadinessFixture(t, root, map[string]string{
		"web/package.json": `{"name":"dirty","version":"1.0.0"}`,
		"dirty/go.mod":     "module example.com/dirty\n",
	})
	ctx := context.Background()
	approved, err := DiscoverReadiness(ctx, root, base)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := DiscoverReadiness(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	var ecosystems []string
	for _, project := range approved {
		ecosystems = append(ecosystems, project.Ecosystem)
	}
	slices.Sort(ecosystems)
	if !slices.Equal(ecosystems, []string{"c", "cpp", "dotnet", "go", "node", "rust"}) || len(checkout) != len(approved)+1 {
		t.Fatalf("approved=%+v checkout=%+v", approved, checkout)
	}
	if strings.Contains(string(approved[0].inputs["web/package.json"]), "dirty") {
		t.Fatal("approved tree used dirty manifest")
	}
	rows, err := CheckReadiness(ctx, approved, "developer", "codex", "codex-native", "dry-run", nil)
	if err != nil || len(rows) != 12 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Status != "unverified" || row.Reason != "dry-run" {
			t.Fatalf("unexpected inspection row %+v", row)
		}
	}
}

func TestReadinessOfflineDependencies(t *testing.T) {
	for _, tc := range []struct{ name, compiler string }{{"c", "cc"}, {"cpp", "c++"}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, executable := range []string{tc.compiler, "ar"} {
				if _, err := exec.LookPath(executable); err != nil {
					t.Skipf("local fixture requires %s: %v", executable, err)
				}
			}
			lib := t.TempDir()
			writeReadinessFixture(t, lib, map[string]string{
				"fixture.h": "extern int fixture(void);\n",
				"fixture.c": "int fixture(void) { return 1; }\n",
			})
			for _, command := range [][]string{{tc.compiler, "-c", "fixture.c", "-o", "fixture.o"}, {"ar", "rcs", "libfixture.a", "fixture.o"}} {
				cmd := exec.Command(command[0], command[1:]...)
				cmd.Dir = lib
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v: %v: %s", command, err, output)
				}
			}
			root := t.TempDir()
			language := "C"
			if tc.name == "cpp" {
				language = "CXX"
			}
			writeReadinessFixture(t, root, map[string]string{"CMakeLists.txt": fmt.Sprintf("project(fixture LANGUAGES %s)\nfind_path(HEADER fixture.h REQUIRED)\nfind_library(LIBRARY fixture REQUIRED)\n", language)})
			projects, err := DiscoverReadiness(context.Background(), root, "")
			if err != nil {
				t.Fatal(err)
			}
			run := func(ctx context.Context, dir, command string, stdout, stderr io.Writer) error {
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CPATH=" + lib, "LIBRARY_PATH=" + lib}
				cmd.Stdout, cmd.Stderr = stdout, stderr
				return cmd.Run()
			}
			check := func() ([]ReadinessRow, error) {
				return CheckReadiness(context.Background(), projects, "developer", "fixture", "fixture", "", run)
			}
			rows, err := check()
			if err != nil || len(rows) != 2 || rows[0].Status != "passed" {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			t.Log(rows[1].Observed)
			if err := os.Remove(filepath.Join(lib, "libfixture.a")); err != nil {
				t.Fatal(err)
			}
			rows, err = check()
			if err == nil || rows[0].Status != "failed" || rows[1].Status != "passed" {
				t.Fatalf("missing library: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func localReadinessExecutor(t *testing.T, env ...string) ReadinessExecutor {
	t.Helper()
	home, tmp := t.TempDir(), t.TempDir()
	makeWritableOnCleanup(t, tmp)
	return func(ctx context.Context, dir, command string, stdout, stderr io.Writer) error {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + tmp}, env...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		return cmd.Run()
	}
}

func TestReadinessOfflineDependenciesGo(t *testing.T) {
	requireGo(t)
	const module = "example.com/readiness@v1.0.0"
	hostGoEnv(t, "off", module)
	repo, base := goModuleRepo(t, module)
	cache := resolvedTempDir(t)
	makeWritableOnCleanup(t, cache)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, cache, PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := DiscoverReadiness(t.Context(), repo, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{true, false} {
		store := deps.Caches[0].Dir
		if !present {
			store = t.TempDir()
		}
		rows, err := CheckReadiness(t.Context(), projects, "developer", "fixture", "fixture", "", localReadinessExecutor(t, "GOMODCACHE="+store))
		if (err == nil) != present || rows[1].Status != "passed" {
			t.Fatalf("present=%t rows=%+v err=%v", present, rows, err)
		}
		t.Log(rows[1].Observed)
	}
	writeReadinessFixture(t, repo, map[string]string{"go.sum": "example.com/readiness v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"})
	checkout, err := DiscoverReadiness(t.Context(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CheckReadiness(t.Context(), checkout, "developer", "fixture", "fixture", "", localReadinessExecutor(t, "GOMODCACHE="+deps.Caches[0].Dir))
	if err == nil || !strings.Contains(rows[0].Reason, "checksum mismatch") {
		t.Fatalf("approved module checksum mismatch was accepted: rows=%+v err=%v", rows, err)
	}

}

func TestReadinessOfflineDependenciesRust(t *testing.T) {
	requireCargo(t)
	fixture := rustRepo(t, "dep")
	hostRustEnv(t)
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, fixture.repo, fixture.base, resolvedTempDir(t), PerRepository)
	if err != nil {
		t.Fatal(err)
	}
	// The shared prefetch fixture deliberately selects a missing SDK; use the
	// default installed toolchain for this offline package test.
	if err := os.Remove(filepath.Join(fixture.repo, "rust-toolchain.toml")); err != nil {
		t.Fatal(err)
	}
	base := gitCommitAll(t, fixture.repo, "use installed toolchain")
	projects, err := DiscoverReadiness(t.Context(), fixture.repo, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fixture.depDir); err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{true, false} {
		store := deps.Caches[0].Dir
		if !present {
			store = t.TempDir()
		}
		run := localReadinessExecutor(t, "CARGO_HOME="+store, "RUSTUP_HOME="+os.Getenv("RUSTUP_HOME"), "RUSTUP_TOOLCHAIN="+os.Getenv("RUSTUP_TOOLCHAIN"))
		rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "fixture", "", run)
		if (err == nil) != present || rows[1].Status != "passed" {
			t.Fatalf("present=%t rows=%+v err=%v", present, rows, err)
		}
		t.Log(rows[1].Observed)
	}
	assertNoSentinels(t, fixture.sentinels)
}

func TestReadinessOfflineDependenciesNode(t *testing.T) {
	requireNpm(t)
	markers, cache, project := t.TempDir(), t.TempDir(), t.TempDir()
	pkg := hookPackage(t, markers)
	pkg.build(t)
	tarball := filepath.Join(t.TempDir(), "hook.tgz")
	if err := os.WriteFile(tarball, pkg.tarball, 0600); err != nil {
		t.Fatal(err)
	}
	root := packageJSON(t, nil, "hook@1.0.0")
	lock := fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"hook":"1.0.0"}},"node_modules/hook":{"version":"1.0.0","resolved":"https://registry.invalid/hook.tgz","integrity":%q,"hasInstallScript":true}}}`, pkg.integrity())
	writeReadinessFixture(t, project, map[string]string{"package.json": root, "package-lock.json": lock})
	var stdout, stderr readinessOutput
	run := localReadinessExecutor(t, "npm_config_cache="+cache)
	if err := run(t.Context(), filepath.Dir(tarball), "npm cache add --ignore-scripts ./hook.tgz", &stdout, &stderr); err != nil {
		t.Fatalf("seed npm cache: %v: %s", err, stderr.data)
	}
	if err := os.Remove(tarball); err != nil {
		t.Fatal(err)
	}
	projects, err := DiscoverReadiness(t.Context(), project, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{true, false} {
		store := cache
		if !present {
			store = t.TempDir()
		}
		rows, err := CheckReadiness(t.Context(), projects, "developer", "fixture", "fixture", "", localReadinessExecutor(t, "npm_config_cache="+store))
		if (err == nil) != present || rows[1].Status != "passed" {
			t.Fatalf("present=%t rows=%+v err=%v", present, rows, err)
		}
		t.Log(rows[1].Observed)
	}
	if entries, err := os.ReadDir(markers); err != nil || len(entries) != 0 {
		t.Fatalf("offline readiness ran lifecycle scripts: %v (%v)", entries, err)
	}
}

func TestReadinessOfflineDependenciesDotnet(t *testing.T) {
	root := t.TempDir()
	writeReadinessFixture(t, root, map[string]string{
		"fixture.csproj": `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup><ItemGroup><PackageReference Include="Fixture.Package" Version="1.2.3" /></ItemGroup></Project>`,
	})
	projects, err := DiscoverReadiness(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	files, source := readinessDotnetInputs(projects[0])
	generated := string(files["readiness-0.csproj"])
	if source != "fixture.csproj" || !strings.Contains(generated, "Fixture.Package") || !strings.Contains(generated, "1.2.3") || !strings.Contains(generated, "net8.0") || !strings.Contains(string(files["NuGet.Config"]), "<clear/>") {
		t.Fatalf("source=%s files=%v", source, files)
	}
	t.Run("local SDK and package", func(t *testing.T) {
		dotnet, err := exec.LookPath("dotnet")
		if err != nil {
			t.Skipf(".NET SDK unavailable: %v", err)
		}
		provision := t.TempDir()
		version, err := exec.Command(dotnet, "--version").Output()
		if err != nil {
			t.Skipf("installed SDK unavailable: %v", err)
		}
		version = []byte(strings.TrimSpace(string(version)))
		t.Logf(".NET SDK %s", version)
		major, _, _ := strings.Cut(string(version), ".")
		framework := "net" + major + ".0"
		writeReadinessFixture(t, provision, map[string]string{
			"package/Fixture.Package.csproj": fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>%s</TargetFramework><PackageId>Fixture.Package</PackageId><Version>1.2.3</Version><NuGetAudit>false</NuGetAudit></PropertyGroup></Project>`, framework),
			"package/NuGet.Config":           `<configuration><packageSources><clear/></packageSources></configuration>`,
			"consumer/fixture.csproj":        strings.ReplaceAll(string(mustReadReadinessFixture(t, filepath.Join(root, "fixture.csproj"))), "net8.0", framework),
			"consumer/global.json":           fmt.Sprintf(`{"sdk":{"version":%q,"rollForward":"disable"}}`, version),
		})
		store := filepath.Join(provision, "packages")
		run := localReadinessExecutor(t, "NUGET_PACKAGES="+store, "DOTNET_CLI_TELEMETRY_OPTOUT=1")
		var out, diagnostics readinessOutput
		if err := run(t.Context(), provision, "dotnet pack ./package/Fixture.Package.csproj -o ./feed && dotnet restore ./consumer/fixture.csproj --source ./feed -p:NuGetAudit=false", &out, &diagnostics); err != nil {
			t.Fatalf("provision package: %v: %s %s", err, out.data, diagnostics.data)
		}
		consumer := filepath.Join(provision, "consumer")
		for _, tc := range []struct {
			name, cache, sdk string
			pass             bool
		}{
			{"available", store, string(version), true},
			{"missing package", t.TempDir(), string(version), false},
			{"missing selected SDK", store, "999.0.100", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				writeReadinessFixture(t, consumer, map[string]string{"global.json": fmt.Sprintf(`{"sdk":{"version":%q,"rollForward":"disable"}}`, tc.sdk)})
				projects, err := DiscoverReadiness(t.Context(), consumer, "")
				if err != nil {
					t.Fatal(err)
				}
				rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "fixture", "", localReadinessExecutor(t, "NUGET_PACKAGES="+tc.cache))
				if (err == nil) != tc.pass || rows[0].Status == "unverified" {
					t.Fatalf("rows=%+v err=%v", rows, err)
				}
			})
		}
	})
}

func mustReadReadinessFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadinessVersionRequirements(t *testing.T) {
	for _, tc := range []struct {
		version, requirement string
		pass, supported      bool
	}{
		{"v20.11.0", "20.11.0", true, true},
		{"v20.11.1", "20.11.0", false, true},
		{"v20.11.1", ">=20.11.0 <21 || ^22.1", true, true},
		{"v21.0.0", ">=20.11.0 <21 || ^22.1", false, true},
		{"v0.2.9", "^0.2.1", true, true},
		{"v0.3.0", "^0.2.1", false, true},
		{"v22.1.0", "22.x", true, true},
		{"v22.2.0", "~22.1.0", false, true},
		{"v22.1.0", "20.0.0 - 22.1.0", false, false},
	} {
		t.Run(tc.version+" "+tc.requirement, func(t *testing.T) {
			pass, supported := checkNodeRange(tc.version, tc.requirement)
			if pass != tc.pass || supported != tc.supported {
				t.Fatalf("pass=%t supported=%t", pass, supported)
			}
		})
	}
	root := t.TempDir()
	writeReadinessFixture(t, root, map[string]string{"package.json": `{"engines":{"node":">=999.0.0"}}`})
	projects, err := DiscoverReadiness(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "fixture", "", func(_ context.Context, _, _ string, out, _ io.Writer) error {
		_, err := io.WriteString(out, "/bin/node\nv20.11.1\n")
		return err
	})
	if err == nil || rows[1].Status != "failed" || !strings.Contains(rows[1].Reason, ">=999.0.0") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestReadinessWorkspaceRoots(t *testing.T) {
	root := t.TempDir()
	writeReadinessFixture(t, root, map[string]string{
		"go.work":                                    "go 1.25\nuse ./go-member\n",
		"go-member/go.mod":                           "module example.com/member\ngo 1.25\n",
		"go-independent/go.mod":                      "module example.com/independent\ngo 1.25\n",
		"web/package.json":                           `{"workspaces":["packages/*"]}`,
		"web/package-lock.json":                      "{}",
		"web/packages/member/package.json":           "{}",
		"web/packages/independent/package-lock.json": "{}",
		"rust/Cargo.toml":                            "[workspace]\nmembers = ['members/*']\n",
		"rust/members/member/Cargo.toml":             "[package]\nname='member'\nversion='0.1.0'\n",
		"rust/members/independent/Cargo.toml":        "[workspace]\n",
		"rust/members/independent/Cargo.lock":        "version = 4\n",
	})
	projects, err := DiscoverReadiness(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, p := range projects {
		roots = append(roots, p.Directory)
	}
	if !slices.Equal(roots, []string{".", "go-independent", "rust", "rust/members/independent", "web", "web/packages/independent"}) {
		t.Fatalf("unexpected workspace roots: %v", roots)
	}
}

func TestReadinessRoleBoundaryPolicies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the native and delegated probe routes are Linux boundaries")
	}
	t.Setenv("CODEX_HOME", "/secret/provider-home")
	t.Setenv("SWARM_GITHUB_TOKEN", "coordinator-secret")
	repo, work, cache := t.TempDir(), t.TempDir(), t.TempDir()
	deps := Dependencies{Caches: []DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: cache}}}
	argv := strings.Join(codexPolicy("/usr/bin/fixture-codex", work, repo, false, nil, deps), "\n")
	for _, expected := range []string{`network={enabled=false}`, strconv.Quote(cache) + `="read"`, `GOMODCACHE=` + strconv.Quote(cache), `inherit="none"`} {
		if !strings.Contains(argv, expected) {
			t.Fatalf("native policy missing %q", expected)
		}
	}
	env := strings.Join(nativeEnv(repo, false, deps), "\n")
	if strings.Contains(env, "CODEX_HOME=") || strings.Contains(env, "coordinator-secret") || !strings.Contains(env, "GOMODCACHE="+cache) {
		t.Fatalf("unexpected native tool environment %q", env)
	}
	flags, err := executorFlags(work, false, nil, deps, "go version", nil)
	if err != nil {
		t.Fatal(err)
	}
	bwrap := strings.Join(flags, " ")
	for _, expected := range []string{"--unshare-all", "--clearenv", "--ro-bind " + cache + " " + cache, "--setenv GOMODCACHE " + cache, "--chdir /workspace", "go version"} {
		if !strings.Contains(bwrap, expected) {
			t.Fatalf("delegated policy missing %q", expected)
		}
	}
	if strings.Contains(bwrap, "coordinator-secret") || strings.Contains(bwrap, "/secret/provider-home") {
		t.Fatalf("delegated tool got host credentials %s", bwrap)
	}
}

func TestReadinessBoundaryExecution(t *testing.T) {
	ctx := t.Context()
	for _, name := range []string{"native", "delegated"} {
		t.Run(name, func(t *testing.T) {
			var binary string
			if name == "native" {
				binary = os.Getenv("SWARM_TEST_CODEX_BINARY")
				if binary == "" {
					t.Skip("set SWARM_TEST_CODEX_BINARY for the real native probe route")
				}
				if err := CheckNativeCodex(ctx, binary); err != nil {
					t.Skipf("native sandbox canaries cannot qualify this test host: %v", err)
				}
			} else {
				requireExecutor(t)
				if err := CheckToolExecutor(ctx); err != nil {
					t.Skipf("Bubblewrap canaries cannot qualify this test host: %v", err)
				}
			}
			isolateRuntimeControl(t)
			useUserCache(t)
			repo := cacheTestRepo(t)
			probeCtx, complete := fixturePreparation(t, repo)
			defer complete()
			store := filepath.Join(t.TempDir(), "go")
			writeReadinessFixture(t, store, map[string]string{"sentinel": "selected-store"})
			work := t.TempDir()
			writeReadinessFixture(t, work, map[string]string{"source": "untouched"})
			opts := WorkerOptions{Role: "tester", Dependencies: Dependencies{Caches: []DependencyCache{{Ecosystem: "go", EnvKey: "GOMODCACHE", Dir: store}}}}
			run := func(command string, out, diagnostics io.Writer) error {
				if name == "native" {
					return RunNativeReadiness(probeCtx, work, binary, command, opts, out, diagnostics)
				}
				return RunDelegatedReadiness(probeCtx, work, command, opts, out, diagnostics)
			}
			var output, diagnostics readinessOutput
			command := `test -z "$SWARM_GITHUB_TOKEN" && cat "$GOMODCACHE/sentinel" && printf changed > source`
			if err := run(command, &output, &diagnostics); err != nil || string(output.data) != "selected-store" {
				t.Fatalf("output=%s diagnostics=%s error=%v", output.data, diagnostics.data, err)
			}
			if err := run("command -v swarm-fixture-nonexistent", io.Discard, io.Discard); err == nil {
				t.Fatal("missing tool passed in selected boundary")
			}
			if source := mustReadReadinessFixture(t, filepath.Join(work, "source")); string(source) != "untouched" {
				t.Fatalf("probe changes escaped the disposable workspace: %s", source)
			}
		})
	}
}
