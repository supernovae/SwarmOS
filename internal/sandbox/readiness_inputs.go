package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/supernovae/swarmos/internal/safefs"
	"gopkg.in/yaml.v3"
)

func readinessProbeInput(name string) bool {
	switch path.Base(name) {
	case "go.sum", "go.work.sum", "Cargo.lock", "CMakeLists.txt", "meson.build", "Makefile", "vcpkg.json", "packages.lock.json", "Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props":
		return true
	}
	switch readinessEcosystem(name) {
	case "go", "node", "rust", "dotnet":
		return true
	}
	return false
}

func checkReadinessDependencies(ctx context.Context, project ReadinessProject, row ReadinessRow, run ReadinessExecutor) ReadinessRow {
	command, source, reason := readinessDependencyCommand(project)
	if command == "" {
		if source != "" {
			row.Source = source
		}
		row.Reason = reason
		return row
	}
	row.Source = source
	dir, err := os.MkdirTemp("", "swarm-readiness-input-*")
	if err == nil {
		defer os.RemoveAll(dir)
		err = writeReadinessInputs(dir, project)
	}
	if err == nil {
		var output, diagnostics readinessOutput
		probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		// The project path is quoted as data and is always relative to the
		// disposable workspace. Command text comes from fixed templates.
		err = run(probeCtx, dir, "cd "+quoteReadinessPath(project.Directory)+" && "+command, &output, &diagnostics)
		cancel()
		row.Observed = strings.TrimSpace(string(output.data))
		if err != nil && len(diagnostics.data) != 0 {
			err = fmt.Errorf("%w (%q)", err, strings.TrimSpace(string(diagnostics.data)))
		}
	}
	if err == nil && project.Ecosystem == "rust" {
		source, reason, pass := rustReadinessVersion(project, row.Observed)
		if !pass {
			row.Source = source
			err = fmt.Errorf("%s", reason)
		} else if strings.HasPrefix(reason, "unsupported") {
			row.Status, row.Reason = "unverified", reason
			return row
		}
	}
	if err == nil && (project.Ecosystem == "c" || project.Ecosystem == "cpp") {
		reason, pass := readinessCMakeVersion(project, row.Observed)
		if !pass {
			err = fmt.Errorf("%s", reason)
		}
	}
	if err != nil {
		row.Status, row.Reason = "failed", "provision the required toolchain and offline packages for the selected role: "+err.Error()
	} else {
		row.Status, row.Reason = "passed", "project requirements check succeeded"
		if project.Ecosystem == "rust" && path.Base(source) != "Cargo.lock" {
			row.Status, row.Reason = "unverified", "Rust toolchain checked. Locked dependency resolution requires Cargo.lock."
		}
	}
	return row
}

// Inspect both source and decoded scalar values because package managers
// interpret escapes before selecting a dependency protocol.
func readinessNodeProtocol(data []byte) bool {
	unsafe := func(value string) bool {
		clean := strings.ToLower(integrityHash.ReplaceAllString(value, ""))
		return containsAny(clean, nodeGitMarkers) || strings.Contains(clean, "exec:")
	}
	if unsafe(string(data)) {
		return true
	}
	var syntax yaml.Node
	if yaml.Unmarshal(data, &syntax) != nil {
		return false
	}
	pending := []*yaml.Node{&syntax}
	for len(pending) > 0 {
		node := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if node.Kind == yaml.ScalarNode && unsafe(node.Value) {
			return true
		}
		pending = append(pending, node.Content...)
	}
	return false
}

func quoteReadinessPath(dir string) string {
	return "'./" + strings.ReplaceAll(dir, "'", "'\"'\"'") + "'"
}

func readinessDependencyCommand(p ReadinessProject) (command, source, reason string) {
	has := func(name string) bool {
		_, ok := p.inputs[path.Join(p.Directory, name)]
		return ok
	}
	reason = "no supported offline dependency declaration was found"
	use := func(name, command string) (string, string, string) {
		if p.Ecosystem == "node" {
			var manifest struct {
				Manager string `json:"packageManager"`
			}
			if data, ok := p.inputs[path.Join(p.Directory, "package.json")]; ok {
				if err := json.Unmarshal(data, &manifest); err != nil {
					return "", "", "invalid package.json"
				}
			}
			if manifest.Manager != "" {
				manager, version, ok := strings.Cut(manifest.Manager, "@")
				version, _, _ = strings.Cut(version, "+")
				expected := "npm"
				switch name {
				case "pnpm-lock.yaml":
					expected = "pnpm"
				case "yarn.lock":
					expected = "yarn"
				}
				if !ok || !packageVer.MatchString(version) {
					return "", "", "unsupported packageManager version declaration"
				}
				if expected != manager {
					command = "printf '%s\\n' 'packageManager does not match the lockfile' >&2; exit 1"
				} else {
					command = "export COREPACK_ENABLE_NETWORK=0; observed=$(" + expected + " --version) || exit; printf '%s\\n' \"$observed\"; test \"$observed\" = '" + version + "' || { printf '%s\\n' 'required " + expected + " " + version + "' >&2; exit 1; }; " + command
				}
			}
		}
		return command, path.Join(p.Directory, name), ""
	}
	switch p.Ecosystem {
	case "go":
		const goEnv = "export GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS= GONOPROXY=none; if test -n \"$GOMODCACHE\"; then export GOPROXY=\"file://$GOMODCACHE/cache/download\" GOMODCACHE=\"$(mktemp -d)\"; fi; "
		if has("go.work") {
			return use("go.work", goEnv+"go mod download all && go list -m -mod=readonly all")
		}
		if has("go.mod") {
			return use("go.mod", goEnv+"export GOWORK=off; go mod download all && go list -m -mod=readonly all")
		}
	case "node":
		for name, data := range p.inputs {
			if readinessEcosystem(name) == "node" && readinessNodeProtocol(data) {
				return "", "", "Git and executable package protocols are unsupported for offline readiness"
			}
		}
		const nodeEnv = "export COREPACK_ENABLE_NETWORK=0 npm_config_ignore_scripts=true npm_config_logs_max=0 npm_config_update_notifier=false npm_config_userconfig=\"$PWD/.readiness-user-npmrc\" npm_config_globalconfig=\"$PWD/.readiness-global-npmrc\"; "
		switch {
		case has("package-lock.json") || has("npm-shrinkwrap.json"):
			name := "package-lock.json"
			if has("npm-shrinkwrap.json") {
				name = "npm-shrinkwrap.json"
			}
			return use(name, nodeEnv+"npm --version && npm ci --offline --ignore-scripts --no-audit --no-fund --engine-strict")
		case has("pnpm-lock.yaml"):
			return use("pnpm-lock.yaml", nodeEnv+"pnpm --version && pnpm install --offline --frozen-lockfile --ignore-scripts --config.package-import-method=copy")
		case has("yarn.lock"):
			if bytes.Contains(p.inputs[path.Join(p.Directory, "yarn.lock")], []byte("__metadata:")) {
				return use("yarn.lock", nodeEnv+"export YARN_ENABLE_NETWORK=0 YARN_ENABLE_SCRIPTS=0 YARN_IGNORE_PATH=1 YARN_ENABLE_GLOBAL_CACHE=0; yarn --version && yarn install --immutable --immutable-cache --mode=skip-build")
			}
			return use("yarn.lock", nodeEnv+"yarn --version && yarn install --offline --frozen-lockfile --ignore-scripts")
		}
		reason = "no supported dependency lockfile was found"
	case "c", "cpp":
		if command, source := readinessCCommand(p); command != "" {
			return command, source, ""
		}
		reason = "no supported required CMake standard, header or library declaration was found"
	case "dotnet":
		files, source := readinessDotnetInputs(p)
		if files != nil {
			if path.Base(source) == "global.json" {
				return "export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1; dotnet --version", source, ""
			}
			return "export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1 DOTNET_CLI_HOME=\"$HOME\"; dotnet --version && for project in ./readiness-*.csproj; do lock=packages.${project#./}; lock=${lock%.csproj}.lock.json; if test -f \"$lock\"; then set -- --locked-mode --lock-file-path \"$lock\"; else set --; fi; dotnet restore \"$project\" --configfile ./NuGet.Config -nodeReuse:false \"$@\" || exit; done", source, ""
		}
		return "", source, "no supported SDK-style framework and package declarations were found"
	case "rust":
		channel, source, reason := rustReadinessSelection(p)
		if reason != "" {
			return "", source, reason
		}
		env := "export CARGO_NET_OFFLINE=true CARGO_NET_GIT_FETCH_WITH_CLI=false RUSTUP_AUTO_INSTALL=0 RUSTC_WRAPPER= RUSTC_WORKSPACE_WRAPPER= CARGO_BUILD_RUSTC_WRAPPER= CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER=; "
		if channel != "" {
			env += "export RUSTUP_TOOLCHAIN='" + channel + "'; "
		}
		version := env + "cargo --version && rustc --version"
		if has("Cargo.toml") && has("Cargo.lock") {
			return use("Cargo.lock", version+" && { if test -n \"$CARGO_HOME\"; then original=$CARGO_HOME; CARGO_HOME=$(mktemp -d) || exit; export CARGO_HOME; for part in registry git; do if test -d \"$original/$part\"; then cp -R \"$original/$part\" \"$CARGO_HOME/$part\" || exit; fi; done; fi; cargo metadata --locked --offline --format-version 1 > /dev/null; }")
		}
		if source != "" || has("Cargo.toml") {
			if source == "" {
				source = path.Join(p.Directory, "Cargo.toml")
			}
			return version, source, ""
		}
		reason = "Cargo.toml and Cargo.lock are required for locked resolution"
	}
	return "", "", reason
}

func writeReadinessInputs(dir string, p ReadinessProject) error {
	root, err := safefs.Open(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if p.Ecosystem == "dotnet" {
		files, _ := readinessDotnetInputs(p)
		return writeReadinessFiles(root, files)
	}
	for name, data := range p.inputs {
		base := path.Base(name)
		eco := readinessEcosystem(name)
		allowed := eco == p.Ecosystem
		switch p.Ecosystem {
		case "go":
			allowed = allowed || base == "go.sum" || base == "go.work.sum"
			if allowed {
				if err := validateReadinessGoInput(p, name, data); err != nil {
					return err
				}
			}
		case "rust":
			allowed = base == "Cargo.toml" || base == "Cargo.lock"
		}
		if !allowed {
			continue
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if base == "package.json" {
			var err error
			data, err = withoutPackageManager(data)
			if err != nil {
				return fmt.Errorf("parse %s: %w", name, err)
			}
		}
		if err := root.WriteFile(name, data, 0600); err != nil {
			return err
		}
		if base == "Cargo.toml" {
			src := path.Join(path.Dir(name), "src")
			if err := root.MkdirAll(src, 0700); err != nil {
				return err
			}
			if err := root.WriteFile(path.Join(src, "lib.rs"), nil, 0600); err != nil {
				return err
			}
		}
	}
	return root.MkdirAll(p.Directory, 0700)
}
