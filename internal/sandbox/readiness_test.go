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
		"web/package.json":                                `{"name":"web","version":"1.0.0"}`,
		"service/go.mod":                                  "module example.com/service\ngo 1.25\n",
		"cli/Cargo.toml":                                  "[package]\nname='fixture'\nversion='0.1.0'\n",
		"sdk/global.json":                                 `{"sdk":{"version":"8.0.100"}}`,
		"native/CMakeLists.txt":                           "project(fixture LANGUAGES C CXX)\n",
		"python/.python-version":                          "3.11\n",
		"python/requirements.txt":                         "wheel==0.45.1\n",
		"maven/pom.xml":                                   "<project/>",
		"gradle/build.gradle.kts":                         "",
		"gradle/gradle.lockfile":                          "example:fixture:1.2.3=runtimeClasspath\n",
		"gradle/gradle/wrapper/gradle-wrapper.properties": "distributionUrl=https://example.invalid/gradle-8.10-bin.zip\n",
	})
	git("add", ".")
	git("-c", "user.name=Readiness", "-c", "user.email=readiness@example.com", "commit", "-qm", "approved")
	base := git("rev-parse", "HEAD")
	writeReadinessFixture(t, root, map[string]string{
		"web/package.json":        `{"name":"dirty","version":"1.0.0"}`,
		"dirty/go.mod":            "module example.com/dirty\n",
		"python/requirements.txt": "dirty-package==2.0\n",
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
	if !slices.Equal(ecosystems, []string{"c", "cpp", "dotnet", "go", "gradle", "maven", "node", "python", "rust"}) || len(checkout) != len(approved)+1 {
		t.Fatalf("approved=%+v checkout=%+v", approved, checkout)
	}
	if strings.Contains(string(approved[0].inputs["web/package.json"]), "dirty") {
		t.Fatal("approved tree used dirty manifest")
	}
	if string(approved[0].inputs["python/requirements.txt"]) != "wheel==0.45.1\n" || string(checkout[0].inputs["python/requirements.txt"]) != "dirty-package==2.0\n" {
		t.Fatal("requirements inspection used the wrong input")
	}
	rows, err := CheckReadiness(ctx, approved, "developer", "codex", "codex-native", "dry-run", nil)
	if err != nil || len(rows) != 2*len(approved) {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Status != "unverified" || row.Reason != "dry-run" || row.Required {
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
				"fixture.h": "extern int fixture(void);\n__attribute__((used)) static int (*fixture_link)(void) = fixture;\n",
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

func readinessRow(t *testing.T, rows []ReadinessRow, check string) ReadinessRow {
	t.Helper()
	for _, row := range rows {
		if row.Check == check {
			return row
		}
	}
	t.Fatalf("missing %s row in %+v", check, rows)
	return ReadinessRow{}
}

func TestReadinessOfflineDependenciesNode(t *testing.T) {
	requireNpm(t)
	if output, err := exec.Command("node", "--version").Output(); err != nil {
		t.Skipf("Node fixture: %v", err)
	} else if ok, _ := checkNodeRange(string(output), ">=24"); !ok {
		t.Skipf("fixture needs Node >=24, found %s", output)
	}
	_, hostCaches := hostNodeEnv(t)
	markers := resolvedTempDir(t)
	pkg := hookPackage(t, markers)
	pkg.build(t)
	tarball := filepath.Join(t.TempDir(), "hook.tgz")
	if err := os.WriteFile(tarball, pkg.tarball, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := packageJSON(t, map[string]any{
		"engines": map[string]string{"node": ">=24"},
		"scripts": map[string]string{"preinstall": markerScript(markers, "root-preinstall")},
	}, "hook@1.0.0")
	lock := fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"hook":"1.0.0"}},"node_modules/hook":{"version":"1.0.0","resolved":"https://registry.invalid/hook.tgz","integrity":%q,"hasInstallScript":true}}}`, pkg.integrity())
	repo, base := nodeRepo(t, map[string]string{"package.json": manifest, "package-lock.json": lock})
	sourceRunner := runPrefetchCommand
	t.Cleanup(func() { runPrefetchCommand = sourceRunner })
	runPrefetchCommand = func(ctx context.Context, dir string, env, argv []string) ([]byte, []byte, error) {
		if !slices.Equal(argv, []string{"npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"}) {
			t.Fatalf("unexpected preparation command: %v", argv)
		}
		if out, diagnostics, err := sourceRunner(ctx, dir, env, []string{"npm", "cache", "add", "--ignore-scripts", tarball}); err != nil {
			t.Fatalf("seed integrity-addressed fixture: %v %s %s", err, out, diagnostics)
		}
		return sourceRunner(ctx, dir, append(env, "npm_config_offline=true"), argv)
	}
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository)
	if err != nil || len(deps.Caches) != 1 {
		t.Fatalf("preparation: %+v, %v", deps, err)
	}
	projects, err := DiscoverReadiness(t.Context(), repo, base)
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(deps.Caches[0].Dir, "npm")
	for _, role := range []string{"developer", "tester"} {
		for _, present := range []bool{true, false} {
			mount := store
			if !present {
				mount = t.TempDir()
			}
			run := localReadinessExecutor(t, "npm_config_cache="+mount)
			check := func(ctx context.Context, dir, command string, out, diagnostics io.Writer) error {
				err := run(ctx, dir, command, out, diagnostics)
				if err == nil && strings.Contains(command, "npm ci") {
					_, err = os.Stat(filepath.Join(dir, "node_modules", "hook", "package.json"))
				}
				return err
			}
			rows, err := CheckReadiness(t.Context(), projects, role, "fixture", "local offline npm", "", check)
			req := readinessRow(t, rows, "requirements")
			if (err == nil) != present || readinessRow(t, rows, "package-manager").Status != "passed" || req.Source != "package-lock.json" ||
				(!present && (!strings.Contains(req.Reason, "hook.tgz") || req.Status != "failed")) {
				t.Fatalf("role=%s present=%t rows=%+v err=%v", role, present, rows, err)
			}
		}
	}
	if entries, err := os.ReadDir(markers); err != nil || len(entries) != 0 {
		t.Fatalf("preparation or offline readiness ran lifecycle scripts: %v (%v)", entries, err)
	}
	for _, input := range []struct{ path, want string }{{"package.json", manifest}, {"package-lock.json", lock}} {
		if data, err := os.ReadFile(filepath.Join(repo, input.path)); err != nil || string(data) != input.want {
			t.Fatalf("preparation changed %s: %v", input.path, err)
		}
	}
	for _, cache := range hostCaches {
		if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
			t.Fatalf("host cache changed: %v (%v)", entries, err)
		}
	}

	// Verify that the fixture would detect execution of the lifecycle hooks.
	control := resolvedTempDir(t)
	writeReadinessFixture(t, control, map[string]string{"package.json": manifest, "package-lock.json": lock})
	var stdout, stderr readinessOutput
	run := localReadinessExecutor(t, "npm_config_cache="+store)
	if err := run(t.Context(), control, "npm ci --offline --no-audit --no-fund --ignore-scripts=false", &stdout, &stderr); err != nil {
		t.Fatalf("scripts-enabled control: %v (%s)", err, stderr.data)
	}
	if _, err := os.Stat(filepath.Join(markers, "root-preinstall")); err != nil {
		t.Fatalf("scripts-enabled control wrote no root marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markers, "hook-install")); err != nil {
		t.Fatalf("scripts-enabled control wrote no package marker: %v", err)
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
	t.Run("offline SDK and package visibility fixture", func(t *testing.T) {
		// Run the production probe through a test SDK that checks the generated
		// restore inputs and resolves the package from the selected store.
		bin, sdk, packages := t.TempDir(), t.TempDir(), t.TempDir()
		const packagePath = "fixture.package/1.2.3/fixture.package.nuspec"
		writeReadinessFixture(t, sdk, map[string]string{
			"sdk/8.0.100/MSBuild.dll": "",
			"packs/Microsoft.NETCore.App.Ref/8.0.0/ref/net8.0/System.Runtime.dll": "",
		})
		writeReadinessFixture(t, packages, map[string]string{
			packagePath: `<package><metadata><id>Fixture.Package</id><version>1.2.3</version></metadata></package>`,
		})
		const dotnet = `#!/bin/sh
set -eu
fail() { printf '%s\n' "$1" >&2; exit 1; }
test -f "$DOTNET_ROOT/sdk/8.0.100/MSBuild.dll" || fail 'SDK 8.0.100 is missing'
case "$1" in
  --version) printf '%s\n' 8.0.100 ;;
  restore)
    test "$#" -eq 5 && test "$2" = ./readiness-0.csproj &&
      test "$3" = --configfile && test "$4" = ./NuGet.Config &&
      test "$5" = -nodeReuse:false || fail 'unexpected restore arguments'
    project=
    IFS= read -r project < "$2" || test -n "$project"
    case "$project" in
      *'<TargetFrameworks>net8.0</TargetFrameworks>'*'<NuGetAudit>false</NuGetAudit>'*'<PackageReference Include="Fixture.Package" Version="1.2.3" />'*) ;;
      *) fail 'unexpected generated project' ;;
    esac
    config=
    IFS= read -r config < "$4" || test -n "$config"
    test "$config" = '<configuration><packageSources><clear/></packageSources></configuration>' ||
      fail 'restore must clear package sources'
    test -f "$DOTNET_ROOT/packs/Microsoft.NETCore.App.Ref/8.0.0/ref/net8.0/System.Runtime.dll" ||
      fail 'net8.0 reference pack is missing'
    package="$NUGET_PACKAGES/fixture.package/1.2.3/fixture.package.nuspec"
    test -f "$package" || fail 'Fixture.Package 1.2.3 is missing from the selected store'
    metadata=
    IFS= read -r metadata < "$package" || test -n "$metadata"
    case "$metadata" in
      *'<id>Fixture.Package</id><version>1.2.3</version>'*) ;;
      *) fail 'unexpected package metadata' ;;
    esac
    ;;
  *) fail 'unexpected SDK command' ;;
esac
`
		if err := os.WriteFile(filepath.Join(bin, "dotnet"), []byte(dotnet), 0700); err != nil {
			t.Fatal(err)
		}
		wrongVersion := t.TempDir()
		writeReadinessFixture(t, wrongVersion, map[string]string{
			"fixture.package/1.2.4/fixture.package.nuspec": `<package><metadata><id>Fixture.Package</id><version>1.2.4</version></metadata></package>`,
		})
		sdkWithoutPack := t.TempDir()
		writeReadinessFixture(t, sdkWithoutPack, map[string]string{"sdk/8.0.100/MSBuild.dll": ""})
		for _, tc := range []struct {
			name, sdk, packages, check, wantFailure string
		}{
			{"available", sdk, packages, "requirements", ""},
			{"missing selected store", sdk, filepath.Join(t.TempDir(), "unmounted"), "requirements", "Fixture.Package 1.2.3 is missing"},
			{"wrong package version", sdk, wrongVersion, "requirements", "Fixture.Package 1.2.3 is missing"},
			{"missing reference pack", sdkWithoutPack, packages, "requirements", "net8.0 reference pack is missing"},
			{"missing selected SDK", t.TempDir(), packages, "toolchain", "SDK 8.0.100 is missing"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, role := range []string{"developer", "tester"} {
					run := localReadinessExecutor(t, "PATH="+bin, "DOTNET_ROOT="+tc.sdk, "NUGET_PACKAGES="+tc.packages)
					rows, err := CheckReadiness(t.Context(), projects, role, "fixture", "offline SDK fixture", "", run)
					row := readinessRow(t, rows, tc.check)
					if tc.wantFailure == "" {
						if err != nil || row.Status != "passed" || row.Observed != "8.0.100" {
							t.Fatalf("%s: rows=%+v err=%v", role, rows, err)
						}
					} else if err == nil || row.Status != "failed" || !strings.Contains(row.Reason, tc.wantFailure) {
						t.Fatalf("%s: expected %q, rows=%+v err=%v", role, tc.wantFailure, rows, err)
					}
					if row.Role != role || row.Source != source || !row.Required {
						t.Fatalf("unexpected readiness row: %+v", row)
					}
				}
			})
		}
	})
	t.Run("local SDK and package", func(t *testing.T) {
		dotnet, err := exec.LookPath("dotnet")
		if err != nil {
			t.Skipf(".NET SDK unavailable: %v", err)
		}
		provision := t.TempDir()
		hostHome := filepath.Join(provision, "host-home")
		versionCmd := exec.CommandContext(t.Context(), dotnet, "--version")
		versionCmd.Dir = provision
		versionCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + hostHome, "DOTNET_CLI_HOME=" + hostHome, "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1"}
		version, err := versionCmd.Output()
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
		// Keep the package in the fixture HOME's default NuGet store for all
		// cases. An empty selected store must fail even with that store present.
		store := filepath.Join(hostHome, ".nuget", "packages")
		run := localReadinessExecutor(t, "NUGET_PACKAGES="+store, "DOTNET_CLI_TELEMETRY_OPTOUT=1")
		var out, diagnostics readinessOutput
		if err := run(t.Context(), provision, "dotnet pack ./package/Fixture.Package.csproj -o ./feed && dotnet restore ./consumer/fixture.csproj --source ./feed -p:NuGetAudit=false", &out, &diagnostics); err != nil {
			t.Fatalf("provision package: %v: %s %s", err, out.data, diagnostics.data)
		}
		if _, err := os.Stat(filepath.Join(store, "fixture.package", "1.2.3", "fixture.package.nuspec")); err != nil {
			t.Fatalf("package is missing from the host-like store: %v", err)
		}
		consumer := filepath.Join(provision, "consumer")
		for _, tc := range []struct {
			name, cache, sdk, wantFailure string
		}{
			{"available", store, string(version), ""},
			{"missing package in selected store", t.TempDir(), string(version), "Fixture.Package (>= 1.2.3)"},
			{"missing selected SDK", store, "999.0.100", "999.0.100"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				writeReadinessFixture(t, consumer, map[string]string{"global.json": fmt.Sprintf(`{"sdk":{"version":%q,"rollForward":"disable"}}`, tc.sdk)})
				projects, err := DiscoverReadiness(t.Context(), consumer, "")
				if err != nil {
					t.Fatal(err)
				}
				run := localReadinessExecutor(t, "HOME="+hostHome, "NUGET_PACKAGES="+tc.cache)
				rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "fixture", "", run)
				row := readinessRow(t, rows, "requirements")
				if tc.wantFailure == "" {
					if err != nil || row.Status != "passed" {
						t.Fatalf("rows=%+v err=%v", rows, err)
					}
				} else if err == nil || !row.Required || row.Status != "failed" || !strings.Contains(row.Observed+"\n"+row.Reason, tc.wantFailure) {
					t.Fatalf("expected %q in failed requirements check, rows=%+v err=%v", tc.wantFailure, rows, err)
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
	home := resolvedTempDir(t)
	t.Setenv("HOME", home)
	maven, gradle := filepath.Join(home, ".m2", "repository"), filepath.Join(home, ".gradle", "caches")
	nuget := filepath.Join(home, ".nuget", "packages")
	t.Setenv("NUGET_PACKAGES", nuget)
	for _, dir := range []string{maven, gradle, nuget} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
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
	for _, store := range []struct{ key, dir string }{
		{"SWARM_MAVEN_REPOSITORY", maven},
		{"SWARM_GRADLE_CACHES", gradle},
		{"NUGET_PACKAGES", nuget},
	} {
		if !strings.Contains(argv, strconv.Quote(store.dir)+`="read"`) || !strings.Contains(argv, store.key+"="+strconv.Quote(store.dir)) ||
			!containsRun(flags, []string{"--ro-bind", store.dir, store.dir}) || !containsRun(flags, []string{"--setenv", store.key, store.dir}) {
			t.Fatalf("selected store %s is not mounted read-only in both policies", store.key)
		}
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

func TestReadinessNodeManagerDiagnostics(t *testing.T) {
	root := t.TempDir()
	writeReadinessFixture(t, root, map[string]string{
		"package.json":      `{"engines":{"node":">=24"},"packageManager":"npm@11.0.0"}`,
		"package-lock.json": `{"lockfileVersion":3}`,
	})
	projects, err := DiscoverReadiness(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ version, check, cause string }{
		{"v22.2.0", "toolchain", "Node >=24"},
		{"v24.1.0", "package-manager", "install npm 11.0.0"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "fixture", "", func(_ context.Context, _, command string, out, _ io.Writer) error {
				switch {
				case strings.Contains(command, "node --version"):
					_, err := fmt.Fprintln(out, "/bin/node\n"+tc.version)
					return err
				case strings.Contains(command, "npm --version"):
					return fmt.Errorf("npm is missing")
				default:
					t.Fatalf("unexpected command %q", command)
					return nil
				}
			})
			row := readinessRow(t, rows, tc.check)
			if err == nil || row.Status != "failed" || row.Source != "package.json" || !strings.Contains(row.Reason, tc.cause) {
				t.Fatalf("row=%+v err=%v", row, err)
			}
		})
	}
}

func TestReadinessOfflineDependenciesPython(t *testing.T) {
	requirePipDownload(t)
	t.Setenv("PIP_INDEX_URL", writePythonIndex(t, resolvedTempDir(t), map[string]map[string][]byte{
		"needed": {"needed-1.0-py3-none-any.whl": testWheel(t, "needed", "1.0")},
	}))
	repo, base := pythonRepo(t, map[string]string{
		"requirements.txt": "needed==1.0\n",
		"pyproject.toml":   "[project]\nrequires-python = '>=3.8'\n",
	})
	deps, err := PrepareDependencyCaches(t.Context(), AgentDependencyCache, repo, base, resolvedTempDir(t), PerRepository)
	if err != nil || len(deps.Caches) != 1 {
		t.Fatalf("wheel preparation: %+v, %v", deps, err)
	}
	projects, err := DiscoverReadiness(t.Context(), repo, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		store, status, reason string
	}{
		{deps.Caches[0].Dir, "passed", "succeeded"},
		{t.TempDir(), "failed", "needed==1.0"},
		{"", "unverified", "set PIP_FIND_LINKS"},
	} {
		rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "offline pip", "", localReadinessExecutor(t, "PIP_FIND_LINKS="+tc.store))
		req := readinessRow(t, rows, "requirements")
		if (err == nil) != (tc.status == "passed") || req.Status != tc.status || !strings.Contains(req.Reason, tc.reason) {
			t.Fatalf("rows=%+v err=%v", rows, err)
		}
	}
	writeReadinessFixture(t, repo, map[string]string{"requirements.txt": "-e .\n"})
	projects, err = DiscoverReadiness(t.Context(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "offline pip", "", localReadinessExecutor(t))
	req := readinessRow(t, rows, "requirements")
	if err == nil || req.Status != "unverified" || !req.Required || req.Source != "requirements.txt" {
		t.Fatalf("unsupported requirement: %+v, %v", req, err)
	}
}

func TestReadinessOfflineDependenciesJava(t *testing.T) {
	for _, tc := range []struct {
		ecosystem, manager, version, storeEnv, missing string
		files                                          map[string]string
		store                                          map[string]string
	}{
		{"maven", "mvn", "Apache Maven 3.9.9", "SWARM_MAVEN_REPOSITORY", "org.example:library:1.0", map[string]string{
			"pom.xml": "<project><dependencies><dependency><groupId>org.example</groupId><artifactId>library</artifactId><version>1.0</version></dependency></dependencies></project>",
		}, map[string]string{
			"org/example/library/1.0/library-1.0.pom": "<project/>",
			"org/example/library/1.0/library-1.0.jar": "jar",
		}},
		{"gradle", "gradle", "Gradle 8.10", "SWARM_GRADLE_CACHES", "org.example:library:1.0", map[string]string{
			"build.gradle.kts":                         "",
			"gradle.lockfile":                          "org.example:library:1.0=runtimeClasspath\n",
			"gradle/wrapper/gradle-wrapper.properties": "distributionUrl=https\\://services.gradle.org/distributions/gradle-8.10-bin.zip\n",
		}, map[string]string{
			"modules-2/files-2.1/org.example/library/1.0/hash/library-1.0.jar": "jar",
		}},
	} {
		t.Run(tc.ecosystem, func(t *testing.T) {
			root, bin, store := t.TempDir(), t.TempDir(), t.TempDir()
			writeReadinessFixture(t, root, tc.files)
			writeReadinessFixture(t, store, tc.store)
			writeReadinessFixture(t, bin, map[string]string{
				"java":     "#!/bin/sh\nprintf '%s\\n' 'openjdk version 21.0.4' >&2\n",
				tc.manager: "#!/bin/sh\nprintf '%s\\n' '" + tc.version + "'\n",
			})
			for _, name := range []string{"java", tc.manager} {
				if err := os.Chmod(filepath.Join(bin, name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			projects, err := DiscoverReadiness(t.Context(), root, "")
			if err != nil || len(projects) != 1 {
				t.Fatalf("projects=%+v err=%v", projects, err)
			}
			for _, present := range []bool{true, false} {
				visible := store
				if !present {
					visible = t.TempDir()
				}
				run := localReadinessExecutor(t, "PATH="+bin+":/usr/bin:/bin", tc.storeEnv+"="+visible)
				rows, err := CheckReadiness(t.Context(), projects, "developer", "fixture", "offline store fixture", "", run)
				tools, req := readinessRow(t, rows, "toolchain"), readinessRow(t, rows, "requirements")
				if (err == nil) != present || !strings.Contains(tools.Observed, tc.version) || !strings.Contains(tools.Observed, "21.0.4") ||
					(!present && !strings.Contains(req.Reason, tc.missing)) {
					t.Fatalf("present=%t rows=%+v err=%v", present, rows, err)
				}
			}
			if tc.ecosystem == "maven" {
				writeReadinessFixture(t, root, map[string]string{"pom.xml": strings.Replace(tc.files["pom.xml"], "1.0</version>", "${library.version}</version>", 1)})
			} else {
				writeReadinessFixture(t, root, map[string]string{"gradle.lockfile": "org.example:library:1.+=runtimeClasspath\n"})
			}
			projects, err = DiscoverReadiness(t.Context(), root, "")
			if err != nil {
				t.Fatal(err)
			}
			rows, err := CheckReadiness(t.Context(), projects, "tester", "fixture", "offline store fixture", "", localReadinessExecutor(t, "PATH="+bin+":/usr/bin:/bin"))
			if req := readinessRow(t, rows, "requirements"); err == nil || req.Status != "unverified" || !req.Required {
				t.Fatalf("dynamic declaration: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestReadinessNodeLockSelection(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, wantLock, wantError string
		locks                               map[string][]byte
	}{
		{"conflict", "{}", "", "competing locks", map[string][]byte{"package-lock.json": {}, "pnpm-lock.yaml": {}}},
		{"explicit selection", `{"packageManager":"pnpm@10.0.0"}`, "pnpm-lock.yaml", "", map[string][]byte{"package-lock.json": {}, "pnpm-lock.yaml": {}}},
		{"missing matching lock", `{"packageManager":"yarn@4.0.0"}`, "", "matching packageManager", map[string][]byte{"package-lock.json": {}}},
		{"shrinkwrap precedence", "{}", "npm-shrinkwrap.json", "", map[string][]byte{"package-lock.json": {}, "npm-shrinkwrap.json": {}}},
		{"wrong Yarn format", `{"packageManager":"yarn@4.0.0"}`, "", "matching the declared Yarn major", map[string][]byte{"yarn.lock": []byte("# yarn lockfile v1\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.locks["package.json"] = []byte(tc.manifest)
			lock, _, _, err := readinessNodeFlow(tc.locks)
			if lock != tc.wantLock || (err != nil) != (tc.wantError != "") || (err != nil && !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("lock=%s, err=%v", lock, err)
			}
		})
	}
}
