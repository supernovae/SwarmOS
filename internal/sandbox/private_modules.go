package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// PrivateGo is the node profile's private Go module allowance for one
// registered repository. Patterns use GOPRIVATE syntax. The zero value
// allows nothing.
type PrivateGo struct {
	Repository string
	Patterns   []string
}

// DependencyPolicy is the node profile's dependency cache policy for one
// repository. It never comes from the repository itself.
type DependencyPolicy struct {
	Mode      DependencyCacheMode
	PrivateGo PrivateGo
}

// DependencyPolicySource accepts a repository policy or a cache mode. A cache
// mode alone never grants access to private modules.
type DependencyPolicySource interface {
	dependencyPolicy() DependencyPolicy
}

func (p DependencyPolicy) dependencyPolicy() DependencyPolicy { return p }

func (m DependencyCacheMode) dependencyPolicy() DependencyPolicy {
	return DependencyPolicy{Mode: m}
}

// privateGoProxy is the credentialed fetch's GOPROXY. Replaced in tests with
// a file:// fixture proxy.
var privateGoProxy = "direct"

// privateGoModuleVersions lists every path@version the base's go.mod
// requires or its go.sum records, sorted and without duplicates.
func privateGoModuleVersions(files map[string][]byte) ([]module.Version, error) {
	seen := map[module.Version]bool{}
	if data, ok := files["go.mod"]; ok {
		f, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return nil, fmt.Errorf("parse approved base go.mod: %w", err)
		}
		for _, r := range f.Require {
			seen[r.Mod] = true
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(files["go.sum"]))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			continue
		}
		seen[module.Version{Path: fields[0], Version: strings.TrimSuffix(fields[1], "/go.mod")}] = true
	}
	versions := make([]module.Version, 0, len(seen))
	for v := range seen {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].Path != versions[j].Path {
			return versions[i].Path < versions[j].Path
		}
		return versions[i].Version < versions[j].Version
	})
	return versions, nil
}

// checkPrivateGoModules is the private module preflight. A module the host
// marks private (GOPRIVATE or GONOPROXY) that no allowed pattern covers is an
// error naming the module and the node setting. It returns the modules the
// allowance covers, which only the credentialed fetch downloads.
func checkPrivateGoModules(getenv func(string) string, files map[string][]byte, allow PrivateGo) ([]string, error) {
	versions, err := privateGoModuleVersions(files)
	if err != nil {
		return nil, err
	}
	hostPrivate := strings.Trim(strings.TrimSpace(getenv("GOPRIVATE"))+","+strings.TrimSpace(getenv("GONOPROXY")), ",")
	allowed := strings.Join(allow.Patterns, ",")
	var fetch, denied []string
	for _, v := range versions {
		switch {
		case allowed != "" && module.MatchPrefixPatterns(allowed, v.Path):
			fetch = append(fetch, v.Path+"@"+v.Version)
		case hostPrivate != "" && module.MatchPrefixPatterns(hostPrivate, v.Path):
			denied = append(denied, v.Path+"@"+v.Version)
		}
	}
	if len(denied) == 0 {
		return fetch, nil
	}
	if len(denied) > 10 {
		denied = append(denied[:10], fmt.Sprintf("and %d more", len(denied)-10))
	}
	noun := "module"
	if len(denied) > 1 {
		noun = "modules"
	}
	if allow.Repository == "" {
		return nil, fmt.Errorf("private Go %s %s not allowed: register this repository (swarm repo add) and list a matching pattern under its name in the node setting private_go_modules", noun, strings.Join(denied, ", "))
	}
	return nil, fmt.Errorf("private Go %s %s not allowed for %s: add a matching pattern to the node setting private_go_modules[%q]", noun, strings.Join(denied, ", "), allow.Repository, allow.Repository)
}

// goPrivateFetchEnv is the environment of the credentialed fetch of allowed
// private modules. It keeps the user's HOME, git configuration, SSH agent and
// Go auth settings, and only those, so the go command can authenticate the
// way it does for the user. SwarmOS and GitHub tokens are never copied. Only
// the allowed patterns are private. GOPROXY alone selects the source, direct
// outside tests, so GONOPROXY is none.
func goPrivateFetchEnv(getenv func(string) string, tmp, cache string, patterns []string) []string {
	private := strings.Join(patterns, ",")
	env := []string{
		"PATH=" + getenv("PATH"), "TMPDIR=" + tmp,
		"GOPATH=" + filepath.Join(tmp, "gopath"), "GOCACHE=" + filepath.Join(tmp, "gocache"),
		"GOMODCACHE=" + cache, "GOPROXY=" + privateGoProxy,
		"GOPRIVATE=" + private, "GONOPROXY=none", "GONOSUMDB=" + private,
		"GOTOOLCHAIN=local", "GOFLAGS=", "GOWORK=off", "GIT_TERMINAL_PROMPT=0",
	}
	for _, key := range []string{
		"HOME", "USER", "XDG_CONFIG_HOME", "GOENV", "GOAUTH", "NETRC", "SSH_AUTH_SOCK", "GIT_SSH", "GIT_SSH_COMMAND",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM",
	} {
		if value := getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

// withPrivatePatterns adds patterns to env's GOPRIVATE, GONOPROXY and
// GONOSUMDB, so the credential-free download never asks a proxy or checksum
// database about an allowed private module.
func withPrivatePatterns(env, patterns []string) []string {
	if len(patterns) == 0 {
		return env
	}
	extra := strings.Join(patterns, ",")
	out := make([]string, 0, len(env)+3)
	set := map[string]bool{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "GOPRIVATE", "GONOPROXY", "GONOSUMDB":
			set[key] = true
			if value != "" {
				entry = key + "=" + value + "," + extra
			} else {
				entry = key + "=" + extra
			}
		}
		out = append(out, entry)
	}
	for _, key := range []string{"GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		if !set[key] {
			out = append(out, key+"="+extra)
		}
	}
	return out
}
