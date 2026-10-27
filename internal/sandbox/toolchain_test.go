package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
		if b.EnvKey == "GOMODCACHE" && b.Ecosystem != "go" {
			t.Errorf("GOMODCACHE bind has ecosystem %q, want go", b.Ecosystem)
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
		bunBin, bunCache, nugetPkgs, dotnetRoot, m2Repo, gradleCaches,
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

	// The user's own ccache is a writable build cache, never a toolchain.
	if _, ok := envVars["CCACHE_DIR"]; ok {
		t.Errorf("CCACHE_DIR discovered from the user's cache: %q", envVars["CCACHE_DIR"])
	}
	for _, m := range mounts {
		if isSubpath(ccacheDir, m) {
			t.Errorf("mount %q exposes the user's ccache %q", m, ccacheDir)
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
	if err := (ToolExecutor{Work: absWT, Dependencies: Dependencies{Mode: SharedHostCaches}}).Run(context.Background(), "go version && go build ./... && go vet ./internal/cli", &out, &out); err != nil {
		t.Fatalf("Bubblewrap offline Go build/vet failed: %v\n%s", err, out.String())
	}
	if binary := os.Getenv("SWARM_TEST_CODEX_BINARY"); binary != "" {
		resolved, err := filepath.EvalSymlinks(binary)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := RunNativeValidation(context.Background(), absWT, resolved, "go build ./... && go test ./... && go vet ./...", nil, Dependencies{Mode: SharedHostCaches}, false, &out, &out); err != nil {
			t.Fatalf("Codex native offline Go build/test/vet failed: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "Refusing to create helper binaries under temporary dir") {
			t.Fatalf("unexpected Codex temporary-dir warning: %s", out.String())
		}
	}
}

func TestIsSafeHomeToolchainPathComponents(t *testing.T) {
	home := "/home/user"
	for _, tc := range []struct {
		rel  string
		want bool
	}{
		{"go/bin", true},
		{"go/bin/x", true},
		{".dotnet", true},
		{".dotnet/tools", true},
		{".local/bin", true},
		{".nvm/versions/node/v22/bin", true},
		{"go/pkg/mod/x", true},
		{"go/binaries", false},
		{".dotnet-x", false},
		{".local/bin-x", false},
		{".nvm-x/bin", false},
		{"go", false},
		{".", false},
		{"../outside", false},
	} {
		if got := isSafeHomeToolchainPath(filepath.Join(home, tc.rel), home); got != tc.want {
			t.Errorf("isSafeHomeToolchainPath(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestToolchainMountForBinKnownLayouts(t *testing.T) {
	home := "/home/user"
	for _, tc := range []struct {
		bin, goos, want string
	}{
		// Known install layouts mount their installation root.
		{home + "/.nvm/versions/node/v22.0.0/bin", "linux", home + "/.nvm/versions/node/v22.0.0"},
		{home + "/.pyenv/versions/3.13.0/bin", "linux", home + "/.pyenv/versions/3.13.0"},
		{home + "/.rustup/toolchains/stable-x86_64-unknown-linux-gnu/bin", "linux", home + "/.rustup/toolchains/stable-x86_64-unknown-linux-gnu"},
		{home + "/.asdf/installs/nodejs/22/bin", "linux", home + "/.asdf/installs/nodejs/22"},
		{home + "/.local/share/mise/installs/go/1.26/bin", "linux", home + "/.local/share/mise/installs/go/1.26"},
		{home + "/.local/share/fnm/node-versions/v22/installation/bin", "linux", home + "/.local/share/fnm/node-versions/v22/installation"},
		{home + "/.fnm/node-versions/v22/installation/bin", "linux", home + "/.fnm/node-versions/v22/installation"},
		{home + "/.volta/tools/image/node/22/bin", "linux", home + "/.volta/tools/image/node/22"},
		{home + "/.sdkman/candidates/java/21/bin", "linux", home + "/.sdkman/candidates/java/21"},
		{"/usr/local/go/bin", "linux", "/usr/local/go"},
		{"/opt/homebrew/bin", "darwin", "/opt/homebrew"},
		{"/opt/homebrew/sbin", "darwin", "/opt/homebrew"},
		{"/home/linuxbrew/.linuxbrew/bin", "linux", "/home/linuxbrew/.linuxbrew"},
		{"/home/linuxbrew/.linuxbrew/sbin", "linux", "/home/linuxbrew/.linuxbrew"},
		{"/usr/local/bin", "darwin", "/usr/local"},
		// Everything else mounts only the bin directory itself.
		{home + "/go/bin", "linux", home + "/go/bin"},
		{"/opt/x/bin", "linux", "/opt/x/bin"},
		{"/opt/x/sbin", "linux", "/opt/x/sbin"},
		{"/usr/local/bin", "linux", "/usr/local/bin"},
		{home + "/.local/bin", "linux", home + "/.local/bin"},
		{home + "/.cargo/bin", "linux", home + "/.cargo/bin"},
		{home + "/.nvm/versions/node/bin", "linux", home + "/.nvm/versions/node/bin"},
		{home + "/.nvm/versions/node/v1/extra/bin", "linux", home + "/.nvm/versions/node/v1/extra/bin"},
		{home + "/.nvm-x/versions/node/v1/bin", "linux", home + "/.nvm-x/versions/node/v1/bin"},
		{"/srv/.nvm/versions/node/v1/bin", "linux", "/srv/.nvm/versions/node/v1/bin"},
		{"/bin", "linux", "/bin"},
		{"/usr/bin", "linux", "/usr/bin"},
	} {
		if got := toolchainMountForBin(tc.bin, home, tc.goos); got != tc.want {
			t.Errorf("toolchainMountForBin(%q, %s) = %q, want %q", tc.bin, tc.goos, got, tc.want)
		}
	}
}

func TestToolchainMountsExcludeGoSrc(t *testing.T) {
	home := resolvedTempDir(t)
	goBin := filepath.Join(home, "go", "bin")
	secret := filepath.Join(home, "go", "src", "private", "secret.txt")
	for _, dir := range []string{goBin, filepath.Dir(secret)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", strings.Join([]string{goBin, "/usr/bin", "/bin"}, string(os.PathListSeparator)))
	for _, key := range []string{"GOROOT", "GOPATH", "GOMODCACHE"} {
		t.Setenv(key, "")
	}
	work, scratch := resolvedTempDir(t), resolvedTempDir(t)

	policy := strings.Join(codexPolicy("/usr/bin/codex", work, scratch, false, nil, Dependencies{}), "\n")
	for _, bad := range []string{filepath.Join(home, "go"), filepath.Join(home, "go", "src")} {
		if strings.Contains(policy, strconv.Quote(bad)+`="read"`) {
			t.Errorf("codex policy mounts %q", bad)
		}
	}
	if !strings.Contains(policy, strconv.Quote(goBin)+`="read"`) {
		t.Errorf("codex policy does not mount %q", goBin)
	}
	for dir := secret; dir != "/"; dir = filepath.Dir(dir) {
		if strings.Contains(policy, strconv.Quote(dir)+`="read"`) {
			t.Errorf("codex policy mounts %q, which exposes %q", dir, secret)
		}
	}

	flags, err := executorFlags(work, false, nil, Dependencies{}, "true", nil)
	if err != nil {
		t.Fatal(err)
	}
	foundGoBin := false
	for i, flag := range flags[:len(flags)-1] {
		if flag != "--ro-bind" {
			continue
		}
		source := flags[i+1]
		if isSubpath(secret, source) {
			t.Errorf("bwrap mounts %q, which exposes %q", source, secret)
		}
		foundGoBin = foundGoBin || source == goBin
	}
	if !foundGoBin {
		t.Errorf("bwrap does not mount %q in %v", goBin, flags)
	}
}
