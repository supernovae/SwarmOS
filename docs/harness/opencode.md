# OpenCode 2 (`opencode`)

OpenCode keeps its own provider login. In isolated mode, SwarmOS starts a private OpenCode server, gives it a dedicated `swarm` agent whose only permitted tools are SwarmOS's, and runs those tools in Bubblewrap.

## Setup

1. Install OpenCode 2.1.2 or newer. SwarmOS uses the V2 configuration and command interfaces.
2. Sign in to your model provider through OpenCode's normal login flow.
3. Install Bubblewrap (`bwrap`), Python 3, Bash, and `prlimit` under `/usr`.
4. Run `swarm repo doctor`.

```yaml
agents:
  opencode:
    binary: opencode
    model: provider/model-name   # optional
```

## How SwarmOS runs OpenCode

| Mode | Invocation |
| --- | --- |
| Isolated | `opencode serve --stdio --hostname 127.0.0.1 --port 0`, then `opencode run --server <private url> --agent swarm` for each turn |
| `trusted-local` | `opencode run --standalone "<prompt>"` unattended, or `opencode mini --standalone --prompt "<task>"` interactively |

The private server listens only on loopback and is protected by a temporary password. That password is between SwarmOS and OpenCode only. It is not a provider credential and never reaches the tool environment. Your shared OpenCode server, if you run one, is not used.

## Boundary

- **Permissions.** The generated configuration denies every tool and integration except the `swarm` MCP server and its `swarm_*` tools. Skills are denied.
- **Checks before work.** SwarmOS waits until the `swarm` MCP server reports connected, rejects any other MCP server in the catalog, and checks the merged agent permissions before the first model request.
- **Configuration discovery.** OpenCode's customization lookup points at a SwarmOS control directory. Provider data stays in its normal location.
- **Files and shell.** Same Bubblewrap executor as Claude Code: detached snapshot at `/workspace`, read-only `/usr`, private temporary storage, clean environment, no network, and read-only source for planners and reviewers.
- **Limits.** Two-minute tool deadlines, per-process resource limits, and 256 MiB of temporary storage per call. Cancelling a run stops detached tool children.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `requires at least 2.1.2` | Upgrade to OpenCode 2. Version 1 configuration is not supported. |
| `OpenCode exposed an unapproved MCP server` | A global MCP server is leaking into the private configuration. Check your OpenCode settings. |
| `OpenCode did not bind a private loopback server` | Make sure loopback networking is available to your user. |

## Qualification

The installed CLI passes an offline model and tool cycle, rejects a forged host-shell call, and stops detached tool children on cancellation. A live authenticated provider run is still pending. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).

Agent budgets default to 60 minutes and can be configured globally or per role in [approved execution policy](../project.md#agent-run-budgets). Interactive native sessions count working time across turns, exclude waits for coordinator input, and have a four-hour wall cap. A warning appears before the deadline. On timeout, native Codex and harness-tools Bubblewrap preserve a bounded recovery patch for [manual inspection](../developer.md#recovering-from-an-interrupted-run). Interactive OCI counts wall time without idle exclusion, and OCI has no timeout patch export. Individual tool-call limits remain separate.
