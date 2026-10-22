package gitcredential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// TokenCommandEnv names a user-authored command that prints the GitHub token on
// stdout (for example `op read op://Private/GitHub/token`). It is read only from
// the operator's environment and never from repository configuration, so a
// repository cannot choose a program that runs on a contributor's machine.
const TokenCommandEnv = "SWARM_GITHUB_TOKEN_COMMAND"

const tokenCommandMaxBytes = 64 << 10
const tokenCommandTimeout = 2 * time.Minute

var tokenEnvKeys = []string{"SWARM_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

var (
	tokenMu     sync.Mutex
	tokenCached bool
	tokenValue  string
	tokenSource string
	tokenErr    error
)

// runTokenCommand executes the configured credential command. Stdin and stderr
// stay attached so password managers can prompt or request biometric unlock.
var runTokenCommand = func(ctx context.Context, command string) ([]byte, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	var out bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, &limitedWriter{dst: &out, remaining: tokenCommandMaxBytes}, os.Stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// GitHubToken resolves the coordinator's GitHub token in precedence order:
// SWARM_GITHUB_TOKEN -> SWARM_GITHUB_TOKEN_COMMAND -> GH_TOKEN -> GITHUB_TOKEN.
// A credential command runs at most once per process and its result is held
// only in memory. An empty token with a nil error means no source is set.
func GitHubToken() (string, string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if !tokenCached {
		tokenValue, tokenSource, tokenErr = resolveGitHubToken()
		tokenCached = true
	}
	return tokenValue, tokenSource, tokenErr
}

func resolveGitHubToken() (string, string, error) {
	explicit := strings.TrimSpace(os.Getenv("SWARM_GITHUB_TOKEN"))
	command := strings.TrimSpace(os.Getenv(TokenCommandEnv))
	if explicit != "" && command != "" {
		return "", "", fmt.Errorf("set only one of SWARM_GITHUB_TOKEN or %s", TokenCommandEnv)
	}
	if explicit != "" {
		return explicit, "SWARM_GITHUB_TOKEN", nil
	}
	if command != "" {
		ctx, cancel := context.WithTimeout(context.Background(), tokenCommandTimeout)
		defer cancel()
		out, err := runTokenCommand(ctx, command)
		if err != nil {
			// Never include captured output: a partial token may be in it.
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", "", fmt.Errorf("%s failed with exit status %d", TokenCommandEnv, exit.ExitCode())
			}
			if ctx.Err() != nil {
				return "", "", fmt.Errorf("%s timed out after %s", TokenCommandEnv, tokenCommandTimeout)
			}
			return "", "", fmt.Errorf("%s failed to run", TokenCommandEnv)
		}
		token := strings.TrimSpace(string(out))
		if token == "" {
			return "", "", fmt.Errorf("%s printed no token", TokenCommandEnv)
		}
		if strings.ContainsAny(token, "\r\n") {
			return "", "", fmt.Errorf("%s must print a single-line token", TokenCommandEnv)
		}
		return token, TokenCommandEnv, nil
	}
	for _, key := range tokenEnvKeys[1:] {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, key, nil
		}
	}
	return "", "", nil
}

func resetTokenCache() {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	tokenCached, tokenValue, tokenSource, tokenErr = false, "", "", nil
}
