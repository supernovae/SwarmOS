package sandbox

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/mod/semver"
)

var (
	rustChannel    = regexp.MustCompile(`^(stable|beta|nightly|[0-9]+\.[0-9]+(?:\.[0-9]+)?)(?:-[0-9]{4}-[0-9]{2}-[0-9]{2})?$`)
	nodeComparator = regexp.MustCompile(`^(>=|<=|>|<|=|~|\^)?[vV]?([0-9]+|[xX*])(?:\.([0-9]+|[xX*]))?(?:\.([0-9]+|[xX*]))?$`)
)

// checkNodeRange handles stable-version npm comparator sets. Prerelease,
// hyphen, and other forms are left for the package manager.
func checkNodeRange(actual, requirement string) (bool, bool) {
	actual = strings.TrimSpace(actual)
	if !semver.IsValid(actual) || semver.Prerelease(actual) != "" {
		return false, false
	}
	passed := false
	for _, alternative := range strings.Split(requirement, "||") {
		fields := strings.Fields(alternative)
		if len(fields) == 0 {
			return false, false
		}
		matches := true
		for _, field := range fields {
			m := nodeComparator.FindStringSubmatch(field)
			if m == nil {
				return false, false
			}
			part := m[2:]
			size := 0
			for size < len(part) && part[size] != "" && !strings.ContainsAny(part[size], "xX*") {
				size++
			}
			if size == 0 {
				if m[1] != "" {
					return false, false
				}
				continue
			}
			for i := size; i < len(part); i++ {
				part[i] = "0"
			}
			base := "v" + strings.Join(part, ".")
			if !semver.IsValid(base) {
				return false, false
			}
			cmp := semver.Compare(actual, base)
			samePrefix := func(n int) bool {
				return strings.Join(strings.Split(actual, ".")[:n], ".") == strings.Join(strings.Split(base, ".")[:n], ".")
			}
			equalRange := func() bool {
				if size == 3 {
					return cmp == 0
				}
				return samePrefix(size)
			}
			match := false
			switch m[1] {
			case ">":
				match = cmp > 0 && (size == 3 || !samePrefix(size))
			case ">=":
				match = cmp >= 0
			case "<":
				match = cmp < 0
			case "<=":
				match = cmp <= 0 || (size < 3 && samePrefix(size))
			case "~":
				match = cmp >= 0 && samePrefix(min(size, 2))
			case "^":
				n := 1
				if size > n && part[0] == "0" {
					n++
					if size > n && part[1] == "0" {
						n++
					}
				}
				match = cmp >= 0 && samePrefix(n)
			default:
				match = equalRange()
			}
			matches = matches && match
		}
		passed = passed || matches
	}
	return passed, true
}

func readinessNodeVersion(project ReadinessProject, observed string) (string, string, bool) {
	source := path.Join(project.Directory, "package.json")
	var manifest struct {
		Engines struct{ Node string }
	}
	data, ok := project.inputs[source]
	if !ok {
		return "", "", true
	}
	if json.Unmarshal(data, &manifest) != nil {
		return source, "unsupported Node manifest or engines declaration", true
	}
	if manifest.Engines.Node == "" {
		return "", "", true
	}
	version := observed[strings.LastIndex(observed, "\n")+1:]
	pass, supported := checkNodeRange(version, manifest.Engines.Node)
	if !supported {
		return source, "unsupported Node engine constraint " + manifest.Engines.Node, true
	}
	return source, "select a role environment satisfying Node " + manifest.Engines.Node, pass
}

func rustReadinessSelection(p ReadinessProject) (channel, source, reason string) {
	for dir := p.Directory; ; dir = path.Dir(dir) {
		tomlName := path.Join(dir, "rust-toolchain.toml")
		legacyName := path.Join(dir, "rust-toolchain")
		if data, ok := p.inputs[legacyName]; ok {
			channel, source = strings.TrimSpace(string(data)), legacyName
			break
		}
		if data, ok := p.inputs[tomlName]; ok {
			var selector struct {
				Toolchain struct {
					Channel    string
					Path       string
					Components []string
					Targets    []string
				}
			}
			if _, err := toml.Decode(string(data), &selector); err != nil || selector.Toolchain.Path != "" || len(selector.Toolchain.Components)+len(selector.Toolchain.Targets) > 0 {
				return "", tomlName, "unsupported Rust toolchain selector"
			}
			channel, source = selector.Toolchain.Channel, tomlName
			break
		}
		if dir == "." {
			break
		}
	}
	if source != "" && !rustChannel.MatchString(channel) {
		return "", source, "unsupported Rust toolchain channel"
	}
	return channel, source, ""
}

func rustReadinessVersion(p ReadinessProject, observed string) (string, string, bool) {
	var actual string
	for _, line := range strings.Split(observed, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "rustc" && semver.IsValid("v"+fields[1]) {
			actual = fields[1]
			break
		}
	}
	channel, selector, _ := rustReadinessSelection(p)
	if channel != "" {
		kind, _, _ := strings.Cut(channel, "-")
		matches := false
		switch kind {
		case "stable":
			matches = actual != "" && semver.Prerelease("v"+actual) == ""
		case "beta", "nightly":
			matches = strings.HasPrefix(semver.Prerelease("v"+actual), "-"+kind)
		default:
			matches = actual == kind || (strings.Count(kind, ".") == 1 && strings.HasPrefix(actual, kind+"."))
		}
		if !matches {
			return selector, "select a role environment providing the declared Rust toolchain " + channel, false
		}
	}
	source := path.Join(p.Directory, "Cargo.toml")
	var manifest struct {
		Package struct {
			Version any `toml:"rust-version"`
		}
	}
	if _, err := toml.Decode(string(p.inputs[source]), &manifest); err != nil || manifest.Package.Version == nil {
		return "", "", true
	}
	version, ok := manifest.Package.Version.(string)
	if !ok || !semver.IsValid("v"+version) {
		return source, "unsupported Cargo rust-version declaration", true
	}
	if actual == "" {
		return source, "Rust version was unavailable in the selected role", false
	}
	return source, "select a role environment with Rust " + version + " or newer", semver.Compare("v"+actual, "v"+version) >= 0
}

func readinessCMakeVersion(p ReadinessProject, observed string) (string, bool) {
	minimum := cmakeMinimumVersion.FindSubmatch(p.inputs[path.Join(p.Directory, "CMakeLists.txt")])
	if minimum == nil {
		return "", true
	}
	for _, line := range strings.Split(observed, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "cmake" && fields[1] == "version" && semver.IsValid("v"+fields[2]) {
			return "select a role environment with CMake " + string(minimum[1]) + " or newer", semver.Compare("v"+fields[2], "v"+string(minimum[1])) >= 0
		}
	}
	return "CMake version was unavailable in the selected role", false
}
