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
    effort: high                  # optional: low, medium, high or max
```

`effort` sets Claude Code's reasoning effort with `--effort`. SwarmOS accepts only the values above and records the effort in each run's provenance. For a different effort per role, add one entry per role (for example `claude-planner` with `harness: claude`) and map it under `roles:`.

## How SwarmOS runs Claude Code

| Mode | Invocation |
| --- | --- |
| Isolated | `claude --restricted --strict-mcp-config --mcp-config <swarm tools> --tools "" --disable-slash-commands --no-chrome --settings <swarm settings> --permission-mode dontAsk --permission-prompts none --print`, plus `--session-id <new id>` for each turn when the installed release lists it, and `--model <model>` and `--effort <effort>` when configured |
| Isolated, interactive | The same settings, one turn at a time. After each turn, SwarmOS asks for a follow-up instruction; press Enter to finish the phase. |
| `trusted-local` | `claude -p --append-system-prompt-file <system prompt> "<task>"` unattended, or the same without `-p` interactively; `--model` and `--effort` come first when configured |

Claude starts in a coordinator-owned control directory, never inside candidate code. Its only way to act on the task is the `swarm` MCP server, which provides `swarm_read`, `swarm_write`, `swarm_edit`, and `swarm_shell`.

## Boundary

Linux native isolated Go commands and worker checks use `GOMAXPROCS=2` and `GOFLAGS=-buildvcs=false -p=2`; see the [shared concurrency defaults](README.md#go-concurrency).

- **Files and shell.** Each tool call runs in Bubblewrap with the detached task snapshot at `/workspace`, read-only system tools under `/usr`, private temporary storage, a clean environment, and separate PID and network namespaces. Planner and reviewer calls get a read-only workspace.
- **Settings.** The generated settings disable all hooks and plugins and allow only `mcp__swarm__*` tools. `--strict-mcp-config` keeps other MCP servers out.
- **Limits.** Each tool call has a two-minute deadline, per-process CPU, address-space, file-size, and descriptor limits, and 256 MiB of private temporary storage. There are no aggregate memory or workspace-disk quotas.

SwarmOS trusts the installed Claude Code to honor its restricted tool selection. Bubblewrap independently confines every tool call it receives.

## Telemetry

After the run, SwarmOS reads the run's own Claude Code session files: `~/.claude/projects/<project>/<session id>.jsonl`, where `<project>` is Claude Code's name for the run's private control directory and each turn's session id is the one SwarmOS passed with `--session-id`. It opens no other file and sends nothing anywhere.

- **Reported:** input tokens (without cache reads), output tokens, cache-read tokens as cached, tool calls and repeated identical shell commands, and the model named in the log.
- **Unavailable:** reasoning tokens and effort, which the log does not carry.

The transcripts stay in your Claude Code history under `~/.claude/projects`, as an interactive session would. SwarmOS never copies, moves or deletes them. If the run's control directory sits behind a symlinked temporary directory and Claude Code names the project differently, the values show as `unavailable`. The session layout was written from Claude Code's documented format and still needs a check against a real 2.1.311 log.

A value the harness does not report stays `unavailable` in `swarm issue status`, `swarm pr status` and the Draft PR evidence. A missing, ambiguous, linked, oversized or malformed record makes every value from it `unavailable` and never fails the run. These figures never affect approval, review, validation or import.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `claude login is unavailable` | Run `claude auth login`, then `swarm repo doctor`. |
| `lacks required interface` | The installed release is missing a flag SwarmOS needs. Update Claude Code. |
| A tool executor check fails | Install `bwrap`, Python 3, Bash, and `prlimit` under `/usr`, and make sure unprivileged user namespaces are enabled. |

## Qualification

An authenticated interactive tool task has passed: synthetic host-file reads were denied, and workspace edits and shell checks worked. A full authenticated WorkUnit is next. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).

Agent budgets default to 60 minutes and can be configured globally or per role in [approved execution policy](../project.md#agent-run-budgets). Interactive native sessions count working time across turns, exclude waits for coordinator input, and have a four-hour wall cap. A warning appears before the deadline. On timeout, native Codex and harness-tools Bubblewrap preserve a bounded recovery patch for [manual inspection](../developer.md#recovering-from-an-interrupted-run). Interactive OCI counts wall time without idle exclusion, and OCI has no timeout patch export. Individual tool-call limits remain separate.
