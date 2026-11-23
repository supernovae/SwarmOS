package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/supernovae/swarmos/internal/safefs"
	"gopkg.in/yaml.v3"
)

const readinessNodeEnv = "export COREPACK_ENABLE_NETWORK=0 COREPACK_ENABLE_AUTO_PIN=0 npm_config_ignore_scripts=true npm_config_logs_max=0 npm_config_update_notifier=false npm_config_userconfig=\"$PWD/.readiness-user-npmrc\" npm_config_globalconfig=\"$PWD/.readiness-global-npmrc\"; "

func readinessLocalInputs(p ReadinessProject) map[string][]byte {
	files := make(map[string][]byte)
	for name, data := range p.inputs {
		if path.Dir(name) == p.Directory {
			files[path.Base(name)] = data
		}
	}
	return files
}

func checkReadinessNodeManager(ctx context.Context, project ReadinessProject, row ReadinessRow, dir string, run ReadinessExecutor) ReadinessRow {
	row.Check, row.Source = "package-manager", path.Join(project.Directory, "package.json")
	row.Status, row.Observed = "unverified", ""
	files := readinessLocalInputs(project)
	lock, manager, version, err := readinessNodeFlow(files)
	if err != nil {
		row.Reason = err.Error()
		return row
	}
	var out, diagnostics readinessOutput
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = run(probeCtx, dir, readinessNodeEnv+manager+" --version", &out, &diagnostics)
	row.Observed = strings.TrimSpace(string(out.data))
	row.Status, row.Reason = "passed", manager+" version check succeeded"
	if err != nil || !nodeManagerMatches(lock, files[lock], row.Observed, version) {
		row.Status, row.Reason = "failed", "install "+strings.TrimSpace(manager+" "+version)+" in the selected role environment"
		if lock == "yarn.lock" && version == "" {
			row.Reason += " with a major version matching " + lock
		}
		if diagnostics.data != nil {
			row.Reason += fmt.Sprintf(" (%q)", strings.TrimSpace(string(diagnostics.data)))
		}
	}
	return row
}

func readinessProbeInput(name string) bool {
	switch path.Base(name) {
	case "gradle-wrapper.properties", "go.sum", "go.work.sum", "Cargo.lock", "CMakeLists.txt", "meson.build", "Makefile", "vcpkg.json", "packages.lock.json", "Directory.Build.props", "Directory.Build.targets", "Directory.Packages.props":
		return true
	}
	switch readinessEcosystem(name) {
	case "go", "node", "rust", "dotnet", "python", "maven", "gradle":
		return true
	}
	return false
}

const readinessPythonWheelStoreUnavailable = "PIP_FIND_LINKS does not identify a visible wheel directory"

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
	if err == nil && project.Ecosystem == "python" {
		if _, ok := project.inputs[path.Join(project.Directory, "uv.lock")]; ok {
			var exported = readinessOutput{limit: maxReadinessInput}
			var diagnostics readinessOutput
			probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			err = run(probeCtx, dir, "cd "+quoteReadinessPath(project.Directory)+" && "+strings.Join(pythonUVExportArgv, " "), &exported, &diagnostics)
			cancel()
			if err != nil {
				err = fmt.Errorf("export uv.lock offline: %w (%q)", err, diagnostics.data)
			} else if exported.truncated || checkRequirements(pythonExportName, exported.data, nil) != nil {
				row.Reason = "uv.lock does not export supported bounded registry requirements; supply compatible locked wheels"
				return row
			} else {
				err = safefs.WriteFile(dir, path.Join(project.Directory, ".readiness-uv.txt"), exported.data, 0600)
			}
		}
	}

	if err == nil {
		var output, diagnostics readinessOutput
		probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		// The project path is quoted as data and is always relative to the
		// disposable workspace. Command text comes from fixed templates.
		err = run(probeCtx, dir, "cd "+quoteReadinessPath(project.Directory)+" && "+command, &output, &diagnostics)
		cancel()
		row.Observed = strings.TrimSpace(string(output.data))
		if err == nil && project.Ecosystem == "python" && row.Observed == readinessPythonWheelStoreUnavailable {
			row.Status, row.Observed = "unverified", ""
			row.Reason = "set PIP_FIND_LINKS to the provisioned offline wheel directory in the selected environment"
			return row
		}
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
		if project.Ecosystem == "node" && project.Directory != "." {
			row.Reason += "; automatic Node preparation uses the repository-root lockfile. Supply a root workspace lock or provision the nested packages in the offline image"
		}
	} else {
		row.Status, row.Reason = "passed", "project requirements check succeeded"
		if project.Ecosystem == "rust" && project.workspaceRoot != "" {
			row.Reason = "Rust toolchain checked; locked crates are checked at workspace root " + project.workspaceRoot
		} else if project.Ecosystem == "rust" && path.Base(source) != "Cargo.lock" {
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
		return command, path.Join(p.Directory, name), ""
	}
	switch p.Ecosystem {
	case "python":
		return readinessPythonCommand(p)
	case "maven", "gradle":
		return readinessJavaCommand(p)
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
		lock, _, _, err := readinessNodeFlow(readinessLocalInputs(p))
		if err != nil {
			return "", path.Join(p.Directory, "package.json"), err.Error()
		}
		switch lock {
		case "package-lock.json", "npm-shrinkwrap.json":
			return use(lock, readinessNodeEnv+"npm ci --offline --ignore-scripts --no-audit --no-fund --engine-strict")
		case "pnpm-lock.yaml":
			return use(lock, readinessNodeEnv+"npm_config_ignore_pnpmfile=true npm_config_manage_package_manager_versions=false pnpm install --offline --frozen-lockfile --ignore-scripts --config.package-import-method=copy")
		case "yarn.lock":
			if bytes.Contains(p.inputs[path.Join(p.Directory, lock)], []byte("__metadata:")) {
				return use(lock, readinessNodeEnv+"export YARN_ENABLE_NETWORK=0 YARN_ENABLE_SCRIPTS=0 YARN_IGNORE_PATH=1 YARN_ENABLE_GLOBAL_CACHE=0; yarn install --immutable --immutable-cache --mode=skip-build")
			}
			return use(lock, readinessNodeEnv+"yarn install --offline --frozen-lockfile --ignore-scripts")
		}

	case "c", "cpp":
		if command, source := readinessCCommand(p); command != "" {
			return command, source, ""
		}
		source := path.Join(p.Directory, "CMakeLists.txt")
		if !has("CMakeLists.txt") {
			source = p.Source
		}
		return "", source, "declare the required CMake standard, header or library with supported static syntax and provision it in the selected environment"
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
	selectedNodeLock := ""
	if p.Ecosystem == "node" {
		selectedNodeLock, _, _, _ = readinessNodeFlow(readinessLocalInputs(p))
	}
	for name, data := range p.inputs {
		base := path.Base(name)
		eco := readinessEcosystem(name)
		allowed := eco == p.Ecosystem
		if p.Ecosystem == "node" {
			allowed = base == "package.json" ||
				(path.Dir(name) == p.Directory && (base == selectedNodeLock || (selectedNodeLock == "pnpm-lock.yaml" && base == "pnpm-workspace.yaml")))
		}
		if p.Ecosystem == "maven" || p.Ecosystem == "gradle" {
			allowed = false
		}
		if p.Ecosystem == "python" {
			allowed = allowed && path.Dir(name) == p.Directory
		}
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

var pythonVersionSpec = regexp.MustCompile(`^(?:[<>=!~]{1,3}[0-9][0-9.]*\*?)(?:,[<>=!~]{1,3}[0-9][0-9.]*\*?)*$`)
var pythonVersionPin = regexp.MustCompile(`^[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)
var pythonRequirementsName = regexp.MustCompile(`^requirements[A-Za-z0-9_.-]*\.txt$`)

func readinessPythonCommand(p ReadinessProject) (string, string, string) {
	files := readinessLocalInputs(p)
	command := "export PIP_CONFIG_FILE=/dev/null; "
	source := p.Source
	checkVersion := func(spec string) {
		command += "python3 -I -c 'import sys; from pip._vendor.packaging.specifiers import SpecifierSet; assert SpecifierSet(sys.argv[1]).contains(\".\".join(map(str, sys.version_info[:3]))), \"requires Python \" + sys.argv[1]' '" + spec + "' || exit; "
	}
	if data, ok := files[".python-version"]; ok {
		pin := strings.TrimSpace(string(data))
		source = path.Join(p.Directory, ".python-version")
		if !pythonVersionPin.MatchString(pin) {
			return "", source, "use a literal Python version in .python-version"
		}
		if strings.Count(pin, ".") == 1 {
			pin += ".*"
		}
		checkVersion("==" + pin)
	}
	if data, ok := files["pyproject.toml"]; ok {
		source = path.Join(p.Directory, "pyproject.toml")
		var manifest struct {
			Project struct {
				RequiresPython string `toml:"requires-python"`
			}
		}
		if _, err := toml.Decode(string(data), &manifest); err != nil {
			return "", source, "supply a valid pyproject.toml with a supported requires-python range"
		}
		spec := strings.ReplaceAll(manifest.Project.RequiresPython, " ", "")
		if spec != "" {
			if !pythonVersionSpec.MatchString(spec) {
				return "", source, "use a supported numeric requires-python range"
			}
			checkVersion(spec)
		}
	}
	var names []string
	for name := range files {
		if matches, _ := path.Match("requirements*.txt", name); matches {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if !pythonRequirementsName.MatchString(name) {
			return "", path.Join(p.Directory, name), "use a conventional requirements filename"
		}
		if err := checkRequirements(name, files[name], files); err != nil {
			return "", path.Join(p.Directory, name), err.Error() + "; supply registry requirements with compatible binary wheels"
		}
	}
	_, uv := files["uv.lock"]
	if uv && files["pyproject.toml"] == nil {
		return "", path.Join(p.Directory, "uv.lock"), "commit pyproject.toml alongside uv.lock"
	}
	if len(names) == 0 && !uv {
		return "", source, "commit supported requirements or uv.lock and prepare compatible binary wheels"
	}
	command += "store=${PIP_FIND_LINKS:-}; test -n \"$store\" && test -d \"$store\" || { printf '%s\\n' '" + readinessPythonWheelStoreUnavailable + "'; exit 0; }; "
	download := strings.Join(pythonPipDownloadArgv, " ") + " --no-index --no-cache-dir --find-links \"$store\" --dest \"$dest\""
	for _, name := range names {
		command += "dest=$(mktemp -d) && " + download + " -r '" + name + "' || { printf '%s\\n' 'wheel resolution failed for " + name + "' >&2; exit 1; }; "
	}
	if uv {
		command += "dest=$(mktemp -d) && " + download + " --no-deps --require-hashes -r .readiness-uv.txt"
		source = path.Join(p.Directory, "uv.lock")
	} else {
		command += ":"
		if len(names) == 1 && source == p.Source {
			source = path.Join(p.Directory, names[0])
		}
	}
	return command, source, ""
}
