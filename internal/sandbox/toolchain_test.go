package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveHostToolchainsDiscoversMultiLanguageCachesWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	mustDir := func(rel string) string {
		t.Helper()
		full := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Clean(resolved)
	}
	mustFile := func(rel string) string {
		t.Helper()
		full := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("secret-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		return full
	}

	// Create language toolchain and package cache directories across ecosystems.
	goModCache := mustDir("go/pkg/mod")
	goBin := mustDir("go/bin")
	rustupHome := mustDir(".rustup")
	cargoBin := mustDir(".cargo/bin")
	cargoReg := mustDir(".cargo/registry")
	cargoGit := mustDir(".cargo/git")
	pipCache := mustDir(".cache/pip")
	uvCache := mustDir(".cache/uv")
	uvPython := mustDir(".local/share/uv/python")
	npmCache := mustDir(".npm")
	pnpmStore := mustDir(".local/share/pnpm/store")
	yarnCache := mustDir(".cache/yarn")
	bunBin := mustDir(".bun/bin")
	bunCache := mustDir(".bun/install/cache")
	nugetPkgs := mustDir(".nuget/packages")
	dotnetRoot := mustDir(".dotnet")
	dotnetTools := mustDir(".dotnet/tools")
	ccacheDir := mustDir(".cache/ccache")
	m2Repo := mustDir(".m2/repository")
	gradleCaches := mustDir(".gradle/caches")
	nvmNodeBin := mustDir(".nvm/versions/node/v22.0.0/bin")
	secretDir := mustDir(".secret-keys/bin")

	// Create sensitive credential files that must NEVER be mounted as roots.
	cargoCreds := mustFile(".cargo/credentials.toml")
	npmrc := mustFile(".npmrc")
	pypirc := mustFile(".pypirc")
	nugetConfig := mustFile(".nuget/NuGet/NuGet.Config")
	m2Settings := mustFile(".m2/settings.xml")
	gradleProps := mustFile(".gradle/gradle.properties")

	envMap := map[string]string{
		"PATH": strings.Join([]string{nvmNodeBin, secretDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)),
	}
	binds := resolveHostToolchains(home, func(key string) string {
		return envMap[key]
	})

	envVars := make(map[string]string)
	for _, b := range binds {
		if b.EnvKey != "" {
			envVars[b.EnvKey] = b.EnvVal
		}
	}

	wantEnv := map[string]string{
		"GOMODCACHE":            goModCache,
		"RUSTUP_HOME":           rustupHome,
		"PIP_CACHE_DIR":         pipCache,
		"UV_CACHE_DIR":          uvCache,
		"UV_PYTHON_INSTALL_DIR": uvPython,
		"npm_config_cache":      npmCache,
		"PNPM_STORE_PATH":       pnpmStore,
		"YARN_CACHE_FOLDER":     yarnCache,
		"BUN_INSTALL_CACHE_DIR": bunCache,
		"NUGET_PACKAGES":        nugetPkgs,
		"DOTNET_ROOT":           dotnetRoot,
		"CCACHE_DIR":            ccacheDir,
	}
	for k, want := range wantEnv {
		if got := envVars[k]; got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}

	mounts := ToolchainMounts(binds, true)
	for _, wantMount := range []string{
		goModCache, goBin, rustupHome, cargoBin, cargoReg, cargoGit,
		pipCache, uvCache, uvPython, npmCache, pnpmStore, yarnCache,
		bunBin, bunCache, nugetPkgs, dotnetRoot, ccacheDir, m2Repo, gradleCaches,
	} {
		covered := false
		for _, m := range mounts {
			if isSubpath(wantMount, m) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("expected mount covering %q in %v", wantMount, mounts)
		}
	}

	// Verify sensitive parent directories or files are NOT mounted.
	cleanHome, _ := filepath.EvalSymlinks(home)
	forbidden := []string{
		cleanHome,
		filepath.Dir(cargoCreds),  // ~/.cargo root (would expose credentials.toml)
		filepath.Dir(nugetConfig), // ~/.nuget/NuGet
		filepath.Dir(nugetPkgs),   // ~/.nuget root
		filepath.Dir(m2Settings),  // ~/.m2 root (would expose settings.xml)
		filepath.Dir(gradleProps), // ~/.gradle root (would expose gradle.properties)
		secretDir,                 // arbitrary unallowlisted ~/.secret-keys/bin on PATH
		npmrc,
		pypirc,
	}
	for _, m := range mounts {
		for _, bad := range forbidden {
			if m == bad || isSubpath(bad, m) {
				t.Errorf("mount %q exposes forbidden path %q", m, bad)
			}
		}
	}

	sandboxPath := SandboxPath(binds)
	pathEntries := filepath.SplitList(sandboxPath)
	for _, wantBin := range []string{goBin, cargoBin, bunBin, dotnetRoot, dotnetTools, nvmNodeBin} {
		if !slices.Contains(pathEntries, wantBin) {
			t.Errorf("SandboxPath %q missing %q", sandboxPath, wantBin)
		}
	}
	if slices.Contains(pathEntries, secretDir) {
		t.Errorf("SandboxPath %q leaked unallowlisted home directory %q", sandboxPath, secretDir)
	}
}

func TestPackWorkerExcludesEphemeralLanguageDirectories(t *testing.T) {
	source, scratch, dest := t.TempDir(), t.TempDir(), t.TempDir()
	writeWorkerFixture(t, source, "src/index.ts", "export const ok = true;\n")
	writeWorkerFixture(t, source, "node_modules/pkg/index.js", "module.exports = {};\n")
	writeWorkerFixture(t, source, ".venv/bin/python", "binary\n")
	writeWorkerFixture(t, source, "__pycache__/mod.pyc", "bytecode\n")
	writeWorkerFixture(t, source, ".pytest_cache/v/cache/nodeids", "[]\n")

	archive := filepath.Join(scratch, "input.tar")
	manifest, err := packWorker(context.Background(), source, archive, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 {
		t.Fatalf("expected only src/index.ts in manifest, got: %#v", manifest)
	}
	if _, ok := manifest["src/index.ts"]; !ok {
		t.Fatalf("missing src/index.ts in manifest: %#v", manifest)
	}
	unpacked, err := unpackWorker(context.Background(), archive, dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpacked) != 1 {
		t.Fatalf("expected 1 unpacked file, got: %#v", unpacked)
	}
}

func TestNativeAndBubblewrapOfflineLanguageBuildAndTest(t *testing.T) {
	if os.Getenv("SWARM_TEST_EXECUTOR") != "1" {
		t.Skip("set SWARM_TEST_EXECUTOR=1 for real sandbox language build tests")
	}
	absWT, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := (ToolExecutor{Work: absWT}).Run(context.Background(), "go version && go build ./... && go vet ./internal/cli", &out, &out); err != nil {
		t.Fatalf("Bubblewrap offline Go build/vet failed: %v\n%s", err, out.String())
	}
	if binary := os.Getenv("SWARM_TEST_CODEX_BINARY"); binary != "" {
		resolved, err := filepath.EvalSymlinks(binary)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := RunNativeValidation(context.Background(), absWT, resolved, "go build ./... && go test ./... && go vet ./...", &out, &out); err != nil {
			t.Fatalf("Codex native offline Go build/test/vet failed: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "Refusing to create helper binaries under temporary dir") {
			t.Fatalf("unexpected Codex temporary-dir warning: %s", out.String())
		}
	}
}
