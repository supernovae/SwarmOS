package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// ToolchainBind represents a verified read-only host directory containing
// language compilers, SDKs, or immutable package caches, along with any
// associated environment variable name/value and PATH binary subdirectories.
// Ecosystem names the dependencyEcosystems entry of a package cache bind.
type ToolchainBind struct {
	EnvKey    string
	EnvVal    string
	Mount     string
	PathDir   string
	Ecosystem string
}

// HostToolchains discovers existing, canonicalized host toolchain and package
// cache directories across supported ecosystems (Go, Rust/Cargo, Python/pip/uv,
// Node/npm/pnpm/Yarn/Bun/nvm/fnm/volta, .NET/NuGet, Java/Maven/Gradle, C/C++
// vcpkg, pyenv/asdf/mise/sdkman/homebrew). Writable build caches are not
// discovered here; see BuildCache. Sandboxes use HostToolchainsFor, which
// drops the host-wide caches of adapter-backed ecosystems.
//
// Only public compiler and package/module cache trees are returned so isolated
// sandboxes and clean validation environments can build, lint, and test offline
// without exposing top-level credential files (such as ~/.npmrc, ~/.pypirc,
// ~/.cargo/credentials.toml, or ~/.nuget/NuGet/NuGet.Config).
func HostToolchains() []ToolchainBind {
	home, _ := os.UserHomeDir()
	return resolveHostToolchains(home, os.Getenv)
}

func resolveHostToolchains(home string, getenv func(string) string) []ToolchainBind {
	var binds []ToolchainBind
	seenMounts := make(map[string]bool)
	seenEnv := make(map[string]bool)

	canonicalDir := func(candidate string) (string, bool) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || !filepath.IsAbs(candidate) {
			return "", false
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", false
		}
		resolved = filepath.Clean(resolved)
		if !filepath.IsAbs(resolved) || resolved == "/" || resolved == "." {
			return "", false
		}
		if home != "" {
			if cleanHome, err := filepath.EvalSymlinks(home); err == nil && resolved == filepath.Clean(cleanHome) {
				return "", false
			}
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return "", false
		}
		return resolved, true
	}

	addCache := func(ecosystem, envKey, rawCandidate string) string {
		dir, ok := canonicalDir(rawCandidate)
		if !ok {
			return ""
		}
		b := ToolchainBind{Ecosystem: ecosystem}
		if !seenMounts[dir] {
			seenMounts[dir] = true
			b.Mount = dir
		}
		if envKey != "" && !seenEnv[envKey] {
			seenEnv[envKey] = true
			b.EnvKey = envKey
			b.EnvVal = dir
		}
		if b.Mount != "" || b.EnvKey != "" {
			binds = append(binds, b)
		}
		return dir
	}

	addToolchainRoot := func(envKey, rawRoot string, binSubdirs ...string) {
		dir, ok := canonicalDir(rawRoot)
		if !ok {
			return
		}
		if envKey != "" && !seenEnv[envKey] {
			seenEnv[envKey] = true
			mount := ""
			if !seenMounts[dir] {
				seenMounts[dir] = true
				mount = dir
			}
			binds = append(binds, ToolchainBind{EnvKey: envKey, EnvVal: dir, Mount: mount})
		} else if !seenMounts[dir] {
			seenMounts[dir] = true
			binds = append(binds, ToolchainBind{Mount: dir})
		}
		for _, sub := range binSubdirs {
			binCandidate := dir
			if sub != "" && sub != "." {
				binCandidate = filepath.Join(dir, sub)
			}
			if binDir, ok := canonicalDir(binCandidate); ok {
				mount := ""
				if !seenMounts[binDir] && !isSubpath(binDir, dir) {
					seenMounts[binDir] = true
					mount = binDir
				}
				binds = append(binds, ToolchainBind{Mount: mount, PathDir: binDir})
			}
		}
	}

	firstCandidate := func(envKey string, fallbacks ...string) string {
		if envKey != "" {
			if v := strings.TrimSpace(getenv(envKey)); v != "" {
				return v
			}
		}
		for _, fb := range fallbacks {
			if fb != "" {
				return fb
			}
		}
		return ""
	}

	xdgCache := strings.TrimSpace(getenv("XDG_CACHE_HOME"))
	if xdgCache == "" && home != "" {
		xdgCache = filepath.Join(home, ".cache")
	}
	xdgData := strings.TrimSpace(getenv("XDG_DATA_HOME"))
	if xdgData == "" && home != "" {
		xdgData = filepath.Join(home, ".local", "share")
	}

	// 1. Go: GOROOT (if custom), GOMODCACHE (modules + downloaded Go toolchains).
	if goroot := strings.TrimSpace(getenv("GOROOT")); goroot != "" {
		addToolchainRoot("GOROOT", goroot, "bin")
	}
	goModCache := strings.TrimSpace(getenv("GOMODCACHE"))
	if goModCache == "" {
		if gopath := strings.TrimSpace(getenv("GOPATH")); gopath != "" {
			first := filepath.SplitList(gopath)[0]
			if first != "" {
				goModCache = filepath.Join(first, "pkg", "mod")
			}
		}
	}
	if goModCache == "" && home != "" {
		goModCache = filepath.Join(home, "go", "pkg", "mod")
	}
	addCache("go", "GOMODCACHE", goModCache)
	if home != "" {
		addToolchainRoot("", filepath.Join(home, "go", "bin"), ".")
	}

	// 2. Rust: RUSTUP_HOME and Cargo public subtrees (excluding credentials.toml).
	rustupFallbacks := []string{}
	if home != "" {
		rustupFallbacks = append(rustupFallbacks, filepath.Join(home, ".rustup"))
	}
	addToolchainRoot("RUSTUP_HOME", firstCandidate("RUSTUP_HOME", rustupFallbacks...))

	cargoHome := strings.TrimSpace(getenv("CARGO_HOME"))
	if cargoHome == "" && home != "" {
		cargoHome = filepath.Join(home, ".cargo")
	}
	if cargoHome != "" {
		addToolchainRoot("", filepath.Join(cargoHome, "bin"), ".")
		addCache("rust", "", filepath.Join(cargoHome, "registry"))
		addCache("rust", "", filepath.Join(cargoHome, "git"))
	}

	// 3. Python: pip, uv, poetry, virtualenv, pyenv, conda.
	pipFallbacks := []string{}
	if xdgCache != "" {
		pipFallbacks = append(pipFallbacks, filepath.Join(xdgCache, "pip"))
	}
	addCache("python", "PIP_CACHE_DIR", firstCandidate("PIP_CACHE_DIR", pipFallbacks...))

	uvCacheFallbacks := []string{}
	if xdgCache != "" {
		uvCacheFallbacks = append(uvCacheFallbacks, filepath.Join(xdgCache, "uv"))
	}
	addCache("python", "UV_CACHE_DIR", firstCandidate("UV_CACHE_DIR", uvCacheFallbacks...))

	uvPythonFallbacks := []string{}
	if xdgData != "" {
		uvPythonFallbacks = append(uvPythonFallbacks, filepath.Join(xdgData, "uv", "python"))
	}
	addCache("python", "UV_PYTHON_INSTALL_DIR", firstCandidate("UV_PYTHON_INSTALL_DIR", uvPythonFallbacks...))

	poetryFallbacks := []string{}
	if xdgCache != "" {
		poetryFallbacks = append(poetryFallbacks, filepath.Join(xdgCache, "pypoetry"))
	}
	addCache("python", "POETRY_CACHE_DIR", firstCandidate("POETRY_CACHE_DIR", poetryFallbacks...))

	pyenvFallbacks := []string{}
	if home != "" {
		pyenvFallbacks = append(pyenvFallbacks, filepath.Join(home, ".pyenv"))
	}
	addToolchainRoot("PYENV_ROOT", firstCandidate("PYENV_ROOT", pyenvFallbacks...), "shims", "bin")

	// 4. Node.js / TypeScript / JS: npm, pnpm, Yarn, Bun, nvm, fnm, volta, Playwright, Cypress.
	npmFallbacks := []string{}
	if home != "" {
		npmFallbacks = append(npmFallbacks, filepath.Join(home, ".npm"))
	}
	if xdgCache != "" {
		npmFallbacks = append(npmFallbacks, filepath.Join(xdgCache, "npm"))
	}
	addCache("node", "npm_config_cache", firstCandidate("npm_config_cache", npmFallbacks...))

	pnpmFallbacks := []string{}
	if xdgData != "" {
		pnpmFallbacks = append(pnpmFallbacks, filepath.Join(xdgData, "pnpm", "store"))
	}
	if home != "" {
		pnpmFallbacks = append(pnpmFallbacks, filepath.Join(home, ".pnpm-store"))
	}
	addCache("node", "PNPM_STORE_PATH", firstCandidate("PNPM_STORE_PATH", pnpmFallbacks...))

	yarnFallbacks := []string{}
	if xdgCache != "" {
		yarnFallbacks = append(yarnFallbacks, filepath.Join(xdgCache, "yarn"))
	}
	if home != "" {
		yarnFallbacks = append(yarnFallbacks, filepath.Join(home, ".yarn", "berry", "cache"))
	}
	addCache("node", "YARN_CACHE_FOLDER", firstCandidate("YARN_CACHE_FOLDER", yarnFallbacks...))

	bunInstall := strings.TrimSpace(getenv("BUN_INSTALL"))
	if bunInstall == "" && home != "" {
		bunInstall = filepath.Join(home, ".bun")
	}
	if bunInstall != "" {
		addToolchainRoot("", filepath.Join(bunInstall, "bin"), ".")
		addCache("node", "BUN_INSTALL_CACHE_DIR", firstCandidate("BUN_INSTALL_CACHE_DIR", filepath.Join(bunInstall, "install", "cache")))
	}

	nvmFallbacks := []string{}
	if home != "" {
		nvmFallbacks = append(nvmFallbacks, filepath.Join(home, ".nvm"))
	}
	addToolchainRoot("NVM_DIR", firstCandidate("NVM_DIR", nvmFallbacks...))

	voltaFallbacks := []string{}
	if home != "" {
		voltaFallbacks = append(voltaFallbacks, filepath.Join(home, ".volta"))
	}
	addToolchainRoot("VOLTA_HOME", firstCandidate("VOLTA_HOME", voltaFallbacks...), "bin")

	if xdgCache != "" {
		addCache("node", "PLAYWRIGHT_BROWSERS_PATH", firstCandidate("PLAYWRIGHT_BROWSERS_PATH", filepath.Join(xdgCache, "ms-playwright")))
		addCache("node", "CYPRESS_CACHE_FOLDER", firstCandidate("CYPRESS_CACHE_FOLDER", filepath.Join(xdgCache, "Cypress")))
	}

	// 5. C / C++ / C# / .NET / Java / JVM / Polyglot managers.
	nugetFallbacks := []string{}
	if home != "" {
		nugetFallbacks = append(nugetFallbacks, filepath.Join(home, ".nuget", "packages"))
	}
	addCache("dotnet", "NUGET_PACKAGES", firstCandidate("NUGET_PACKAGES", nugetFallbacks...))

	dotnetFallbacks := []string{}
	if home != "" {
		dotnetFallbacks = append(dotnetFallbacks, filepath.Join(home, ".dotnet"))
	}
	addToolchainRoot("DOTNET_ROOT", firstCandidate("DOTNET_ROOT", dotnetFallbacks...), ".", "tools")

	// Build caches such as ccache come from the SwarmOS build cache table
	// (build_cache.go), never from the user's own cache directories.

	vcpkgFallbacks := []string{}
	if xdgCache != "" {
		vcpkgFallbacks = append(vcpkgFallbacks, filepath.Join(xdgCache, "vcpkg"))
	}
	addCache("c-cpp", "VCPKG_DEFAULT_BINARY_CACHE", firstCandidate("VCPKG_DEFAULT_BINARY_CACHE", vcpkgFallbacks...))

	if home != "" {
		addCache("maven", "", filepath.Join(home, ".m2", "repository"))
		addCache("gradle", "", filepath.Join(home, ".gradle", "caches"))
		addCache("gradle", "", filepath.Join(home, ".gradle", "wrapper"))
		addToolchainRoot("SDKMAN_DIR", firstCandidate("SDKMAN_DIR", filepath.Join(home, ".sdkman")))
		addToolchainRoot("ASDF_DATA_DIR", firstCandidate("ASDF_DATA_DIR", filepath.Join(home, ".asdf")), "shims", "bin")
		addToolchainRoot("", filepath.Join(home, ".local", "bin"), ".")
	}
	if xdgData != "" {
		addToolchainRoot("MISE_DATA_DIR", firstCandidate("MISE_DATA_DIR", filepath.Join(xdgData, "mise")), "shims")
	}

	// System-wide language toolchain locations outside /usr/bin (e.g., /usr/local, /opt, Homebrew).
	for _, sysPath := range []string{"/usr/local/go", "/home/linuxbrew/.linuxbrew", "/opt/homebrew"} {
		addToolchainRoot("", sysPath, "bin", "sbin")
	}

	// Also preserve any existing non-home compiler directories already on host PATH
	// (for example, nvm/fnm/asdf/mise/nix/usr-local versioned bin directories).
	cleanHome := ""
	if home != "" {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			cleanHome = filepath.Clean(resolved)
		}
	}
	for _, entry := range filepath.SplitList(getenv("PATH")) {
		dir, ok := canonicalDir(entry)
		if !ok {
			continue
		}
		if strings.HasPrefix(dir, "/mnt/") || dir == "/usr/bin" || dir == "/bin" || dir == "/usr/sbin" || dir == "/sbin" {
			continue
		}
		// Only admit known toolchain/version-manager subdirectories under HOME,
		// never arbitrary user dot-directories that might hold credentials.
		if cleanHome != "" && isSubpath(dir, cleanHome) && !isSafeHomeToolchainPath(dir, cleanHome) {
			continue
		}
		rootMount := toolchainMountForBin(dir, cleanHome, runtime.GOOS)
		mount := ""
		if !seenMounts[rootMount] {
			seenMounts[rootMount] = true
			mount = rootMount
		}
		binds = append(binds, ToolchainBind{Mount: mount, PathDir: dir})
	}

	return binds
}

func isSubpath(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func isSafeHomeToolchainPath(dir, cleanHome string) bool {
	rel, err := filepath.Rel(cleanHome, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	rel = filepath.ToSlash(rel)
	safePrefixes := []string{
		"go/bin",
		"go/pkg/mod/",
		".cargo/bin",
		".rustup/",
		".nvm/",
		".fnm/",
		".volta/",
		".bun/bin",
		".pyenv/",
		".asdf/",
		".sdkman/",
		".dotnet",
		".local/bin",
		".local/share/mise/",
		".local/share/fnm/",
		".local/share/uv/",
		".local/share/pnpm/",
	}
	for _, prefix := range safePrefixes {
		if hasPathComponents(rel, strings.TrimSuffix(prefix, "/")) {
			return true
		}
	}
	return false
}

// hasPathComponents reports whether the slash-separated components of prefix
// are the leading components of rel, so "go/bin" matches "go/bin/x" but not
// "go/binaries".
func hasPathComponents(rel, prefix string) bool {
	relParts, prefixParts := strings.Split(rel, "/"), strings.Split(prefix, "/")
	return len(relParts) >= len(prefixParts) && slices.Equal(relParts[:len(prefixParts)], prefixParts)
}

// Known install layouts whose bin directory must be mounted with its parent so
// relative `../lib` and `../share` lookups inside the toolchain work. A `*`
// matches exactly one path component. Home layouts are relative to the
// canonical HOME.
var (
	homeInstallLayouts = []string{
		".nvm/versions/node/*/bin",
		".pyenv/versions/*/bin",
		".rustup/toolchains/*/bin",
		".asdf/installs/*/*/bin",
		".local/share/mise/installs/*/*/bin",
		".local/share/fnm/node-versions/*/installation/bin",
		".fnm/node-versions/*/installation/bin",
		".volta/tools/image/*/*/bin",
		".sdkman/candidates/*/*/bin",
	}
	systemInstallLayouts = []string{
		"/usr/local/go/bin",
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/home/linuxbrew/.linuxbrew/sbin",
	}
	// Intel-Mac Homebrew links /usr/local/bin into /usr/local/Cellar.
	darwinInstallLayouts = []string{
		"/usr/local/bin",
		"/usr/local/sbin",
	}
)

// toolchainMountForBin returns the enclosing installation directory for a bin
// directory that matches a known install layout (such as `/usr/local/go` for
// `/usr/local/go/bin` or an nvm node version root for
// `~/.nvm/versions/node/v22.0.0/bin`). Any other directory, including
// `~/go/bin` and `/opt/<x>/bin`, is returned unchanged so its siblings are
// never exposed.
func toolchainMountForBin(binDir, cleanHome, goos string) string {
	layouts := systemInstallLayouts
	if goos == "darwin" {
		layouts = append(slices.Clip(layouts), darwinInstallLayouts...)
	}
	for _, layout := range layouts {
		if matchLayout(binDir, layout) {
			return filepath.Dir(binDir)
		}
	}
	if cleanHome == "" || !isSubpath(binDir, cleanHome) || !isSafeHomeToolchainPath(binDir, cleanHome) {
		return binDir
	}
	rel, err := filepath.Rel(cleanHome, binDir)
	if err != nil {
		return binDir
	}
	for _, layout := range homeInstallLayouts {
		if matchLayout(filepath.ToSlash(rel), layout) {
			return filepath.Dir(binDir)
		}
	}
	return binDir
}

func matchLayout(path, layout string) bool {
	pathParts, layoutParts := strings.Split(path, "/"), strings.Split(layout, "/")
	if len(pathParts) != len(layoutParts) {
		return false
	}
	for i, part := range layoutParts {
		if part != "*" && part != pathParts[i] {
			return false
		}
	}
	return true
}

// SandboxPath constructs the deterministic PATH string for sandboxed commands,
// combining discovered toolchain binary directories with standard system paths.
func SandboxPath(binds []ToolchainBind) string {
	seen := make(map[string]bool)
	var parts []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		parts = append(parts, p)
	}
	for _, b := range binds {
		add(b.PathDir)
	}
	if runtime.GOOS == "windows" {
		return strings.Join(parts, string(os.PathListSeparator))
	}
	for _, std := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		add(std)
	}
	return strings.Join(parts, ":")
}

// ToolchainMounts returns deduplicated read-only directory mounts for sandboxes.
// It filters out mounts that are already covered by an ancestor mount or by standard
// root mounts (/usr, /bin, /lib, /lib64).
func ToolchainMounts(binds []ToolchainBind, skipSystemUsr bool) []string {
	var raw []string
	seen := make(map[string]bool)
	for _, b := range binds {
		m := b.Mount
		if m == "" || seen[m] {
			continue
		}
		if skipSystemUsr && (m == "/usr" || isSubpath(m, "/usr") || m == "/bin" || m == "/lib" || m == "/lib64") {
			continue
		}
		seen[m] = true
		raw = append(raw, m)
	}
	var filtered []string
	for i, m := range raw {
		covered := false
		for j, other := range raw {
			if i != j && m != other && isSubpath(m, other) {
				covered = true
				break
			}
		}
		if !covered {
			filtered = append(filtered, m)
		}
	}
	return filtered
}
