# Claude Code (`claude`)

Claude Code keeps your normal Claude login. In isolated mode, SwarmOS starts it in restricted mode with its built-in tools turned off and gives it a SwarmOS tool server instead. Those tools run inside Bubblewrap.

## Setup

1. Install Claude Code 2.1.308 or newer.
2. Sign in with `claude auth login`. SwarmOS checks `claude auth status` before each isolated run.
3. Install Bubblewrap (`bwrap`), Python 3, Bash, and `prlimit` under `/usr`.
4. Run `swarm repo doctor`.

```yaml
agents:
  claude:
    binary: claude
    model: your-preferred-model   # optional
```

## How SwarmOS runs Claude Code

| Mode | Invocation |
| --- | --- |
| Isolated | `claude --restricted --strict-mcp-config --mcp-config <swarm tools> --tools "" --disable-slash-commands --no-chrome --settings <swarm settings> --permission-mode dontAsk --permission-prompts none --no-session-persistence --print` |
| Isolated, interactive | The same settings, one turn at a time. After each turn, SwarmOS asks for a follow-up instruction; press Enter to finish the phase. |
| `trusted-local` | `claude -p --append-system-prompt-file <system prompt> "<task>"` unattended, or the same without `-p` interactively |

Claude starts in a coordinator-owned control directory, never inside candidate code. Its only way to act on the task is the `swarm` MCP server, which provides `swarm_read`, `swarm_write`, `swarm_edit`, and `swarm_shell`.

## Boundary

- **Files and shell.** Each tool call runs in Bubblewrap with the detached task snapshot at `/workspace`, read-only system tools under `/usr`, private temporary storage, a clean environment, and separate PID and network namespaces. Planner and reviewer calls get a read-only workspace.
- **Settings.** The generated settings disable all hooks and plugins and allow only `mcp__swarm__*` tools. `--strict-mcp-config` keeps other MCP servers out.
- **Limits.** Each tool call has a two-minute deadline, per-process CPU, address-space, file-size, and descriptor limits, and 256 MiB of private temporary storage. There are no aggregate memory or workspace-disk quotas.

SwarmOS trusts the installed Claude Code to honor its restricted tool selection. Bubblewrap independently confines every tool call it receives.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `claude login is unavailable` | Run `claude auth login`, then `swarm repo doctor`. |
| `lacks required interface` | The installed release is missing a flag SwarmOS needs. Update Claude Code. |
| A tool executor check fails | Install `bwrap`, Python 3, Bash, and `prlimit` under `/usr`, and make sure unprivileged user namespaces are enabled. |

## Qualification

An authenticated interactive tool task has passed: synthetic host-file reads were denied, and workspace edits and shell checks worked. A full authenticated WorkUnit is next. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).
