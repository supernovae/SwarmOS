// Package gitcredential confines explicit GitHub credentials to the coordinator's
// Git child environment. Workers must separately be isolated from that process.
package gitcredential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Configure avoids exposing credentials in process arguments or Git config files.
// The returned function redacts the credential from subprocess diagnostics.
func Configure(cmd *exec.Cmd) (func(string) string, error) {
	token, _, err := GitHubToken()
	if err != nil {
		return nil, err
	}
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_CONFIG_") || strings.HasPrefix(upper, "GIT_TRACE") || upper == "GIT_CURL_VERBOSE" || upper == "GIT_EXTERNAL_DIFF" || upper == "GIT_DIFF_OPTS" || upper == "GIT_REPLACE_REF_BASE" || upper == "GIT_NO_REPLACE_OBJECTS" ||
			(token != "" && (upper == "SWARM_GITHUB_TOKEN" || upper == "GH_TOKEN" || upper == "GITHUB_TOKEN")) || upper == TokenCommandEnv || upper == "SWARM_GITHUB_FORK_READ_TOKEN" {
			continue
		}
		env = append(env, item)
	}
	config := [][2]string{
		{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"},
		{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"},
		{"gc.auto", "0"}, {"maintenance.auto", "false"},
	}
	if token != "" {
		basic := basicAuth(token)
		config = append(config, [2]string{"credential.helper", ""}, [2]string{"http.https://github.com/.extraheader", "AUTHORIZATION: basic " + basic})
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, entry := range config {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+entry[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+entry[1])
	}
	cmd.Env = append(env, "GIT_NO_REPLACE_OBJECTS=1")
	return redactionFor(token).Redact, nil
}

// Command constructs a coordinator Git command. Working-tree operations disable
// every configured filter so candidate attributes cannot invoke host programs.
func Command(ctx context.Context, root string, args ...string) (*exec.Cmd, func(string) string, error) {
	args = append([]string(nil), args...)
	if len(args) > 0 && (args[0] == "diff" || args[0] == "show" || args[0] == "log") {
		args = append([]string{args[0], "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	redact, err := Configure(cmd)
	if err != nil {
		return nil, nil, err
	}
	if len(args) == 0 {
		return cmd, redact, nil
	}
	if args[0] == "merge-tree" {
		probe := exec.CommandContext(ctx, "git", "config", "--null", "--name-only", "--get-regexp", `^merge\..*\.driver$`)
		probe.Dir, probe.Env = root, cmd.Env
		var keys bytes.Buffer
		probe.Stdout = &limitedWriter{dst: &keys, remaining: 1 << 20}
		if err := probe.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				return nil, nil, fmt.Errorf("inspect Git merge drivers: %w", err)
			}
		}
		for _, key := range strings.Split(keys.String(), "\x00") {
			if key == "" {
				continue
			}
			appendConfig(cmd, key, "false")
			appendConfig(cmd, strings.TrimSuffix(key, ".driver")+".recursive", "binary")
		}
	}
	switch args[0] {
	case "add", "commit", "status", "worktree", "checkout", "reset", "diff", "merge-tree":
		probe := exec.CommandContext(ctx, "git", "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
		probe.Dir = root
		probe.Env = cmd.Env
		var keys bytes.Buffer
		probe.Stdout = &limitedWriter{dst: &keys, remaining: 1 << 20}
		err := probe.Run()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				return nil, nil, fmt.Errorf("inspect Git filters: %w", err)
			}
		}
		seen := map[string]bool{}
		for _, key := range strings.Split(keys.String(), "\x00") {
			if key == "" {
				continue
			}
			dot := strings.LastIndexByte(key, '.')
			if dot < len("filter.") {
				return nil, nil, fmt.Errorf("invalid Git filter key")
			}
			name := key[:dot]
			if seen[name] {
				continue
			}
			seen[name] = true
			for _, attr := range []string{"clean", "smudge", "process", "required"} {
				value := ""
				if attr == "required" {
					value = "false"
				}
				appendConfig(cmd, name+"."+attr, value)
			}
		}
	}
	return cmd, redact, nil
}

func appendConfig(cmd *exec.Cmd, key, value string) {
	for i, entry := range cmd.Env {
		if strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			count, _ := strconv.Atoi(strings.TrimPrefix(entry, "GIT_CONFIG_COUNT="))
			cmd.Env[i] = "GIT_CONFIG_COUNT=" + strconv.Itoa(count+1)
			cmd.Env = append(cmd.Env, "GIT_CONFIG_KEY_"+strconv.Itoa(count)+"="+key, "GIT_CONFIG_VALUE_"+strconv.Itoa(count)+"="+value)
			return
		}
	}
}

type limitedWriter struct {
	dst       *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("git filter configuration exceeds byte budget")
	}
	w.remaining -= len(p)
	return w.dst.Write(p)
}

// Output runs a coordinator command and sanitizes failure diagnostics. Successful
// object contents are returned byte-for-byte so policy digests remain stable.
func Output(ctx context.Context, root string, args ...string) ([]byte, error) {
	if IsNetwork(args) {
		out, err := Network(ctx, root, false, args...)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	cmd, redact, err := Command(ctx, root, args...)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", redact(strings.Join(args, " ")), err)
	}
	return out, nil
}

// ForkRead permits only exact repository branch lookup and commit fetch. It uses
// the same bounded engine as coordinator writes, with independent credentials.
func ForkRead(ctx context.Context, root, host, slug, token, operation, value string) ([]byte, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`).MatchString(host) || !regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+$`).MatchString(slug) || strings.Contains(slug, "..") {
		return nil, fmt.Errorf("invalid fork read destination")
	}
	destination := (&url.URL{Scheme: "https", Host: host, Path: "/" + slug + ".git"}).String()
	return forkRead(ctx, root, destination, token, operation, value)
}

func forkRead(ctx context.Context, root, destination, token, operation, value string) ([]byte, error) {
	var args []string
	switch operation {
	case "ls-remote":
		if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, " ~^:?*[\\\\") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "//") || value == "@" {
			return nil, fmt.Errorf("invalid fork branch")
		}
		for _, part := range strings.Split(value, "/") {
			if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
				return nil, fmt.Errorf("invalid fork branch")
			}
		}
		for _, r := range value {
			if r < 33 || r == 127 {
				return nil, fmt.Errorf("invalid fork branch")
			}
		}
		args = []string{"ls-remote", "--refs", destination, "refs/heads/" + value}
	case "fetch":
		if !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(value) {
			return nil, fmt.Errorf("invalid fork commit")
		}
		args = []string{"fetch", "--no-write-fetch-head", destination, value}
	default:
		return nil, fmt.Errorf("unsupported fork read operation")
	}
	out, err := network(ctx, root, false, func(ctx context.Context) (*exec.Cmd, func(string) string, error) {
		// Do not resolve the normal writer credential, even for configuration probes.
		var env []string
		for _, item := range os.Environ() {
			key, _, _ := strings.Cut(item, "=")
			key = strings.ToUpper(key)
			if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "SWARM_GITHUB_") || key == "GH_TOKEN" || key == "GITHUB_TOKEN" || key == "SSH_AUTH_SOCK" {
				continue
			}
			env = append(env, item)
		}
		env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS="+os.DevNull, "GIT_NO_REPLACE_OBJECTS=1")
		probe := exec.CommandContext(ctx, "git", "config", "--null", "--name-only", "--get-regexp", `^(url\..*|credential\..*|http\..*|core\.(gitproxy|sshcommand|askpass|alternaterefscommand)|fetch\..*|submodule\..*|extensions\.partialclone|remote\..*://.*|remote\..*\.(vcs|uploadpack|receivepack|proxy|promisor|partialclonefilter)|include.*)$`)
		probe.Dir, probe.Env = root, env
		var keys bytes.Buffer
		probe.Stdout = &limitedWriter{dst: &keys, remaining: 1 << 20}
		err := probe.Run()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				return nil, nil, fmt.Errorf("cannot inspect fork read configuration")
			}
		}
		if keys.Len() != 0 {
			return nil, nil, fmt.Errorf("fork read rejects repository transport configuration")
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		cmd.Env = append(env, "GIT_CONFIG_COUNT=0")
		for _, pair := range [][2]string{{"core.hooksPath", os.DevNull}, {"core.fsmonitor", "false"}, {"credential.helper", ""}, {"http.followRedirects", "false"}, {"http.extraHeader", ""}, {"http." + destination + ".extraHeader", "AUTHORIZATION: basic " + basicAuth(token)}, {"protocol.allow", "never"}, {"protocol.https.allow", "always"}, {"protocol.http.allow", "always"}, {"gc.auto", "0"}, {"maintenance.auto", "false"}, {"fetch.recurseSubmodules", "false"}, {"submodule.recurse", "false"}, {"fetch.prune", "false"}, {"fetch.pruneTags", "false"}, {"fetch.writeCommitGraph", "false"}} {
			appendConfig(cmd, pair[0], pair[1])
		}
		return cmd, redactionFor(token).Redact, nil
	}, operation)
	redact := redactionFor(token).Redact
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		exit.Stderr = []byte(redact(string(exit.Stderr)))
	}
	return []byte(redact(string(out))), err
}
