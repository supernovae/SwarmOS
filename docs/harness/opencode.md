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
| Isolated | `opencode serve --stdio --hostname 127.0.0.1 --port 0`, then `opencode api --server <private url> session.create` and `opencode run --server <private url> --agent swarm --session <id>` for each turn. If the session cannot be created in the run's control directory, or `run --help` does not list `--session`, follow-up turns use `--continue` instead |
| `trusted-local` | `opencode run --standalone "<prompt>"` unattended, or `opencode mini --standalone --prompt "<task>"` interactively |

The private server listens only on loopback and is protected by a temporary password. That password is between SwarmOS and OpenCode only. It is not a provider credential and never reaches the tool environment. Your shared OpenCode server, if you run one, is not used.

## Boundary

Linux native isolated Go commands and worker checks use `GOMAXPROCS=2` and `GOFLAGS=-buildvcs=false -p=2`; see the [shared concurrency defaults](README.md#go-concurrency).

- **Permissions.** The generated configuration denies every tool and integration except the `swarm` MCP server and its `swarm_*` tools. Skills are denied.
- **Checks before work.** SwarmOS waits until the `swarm` MCP server reports connected, rejects any other MCP server in the catalog, and checks the merged agent permissions before the first model request.
- **Configuration discovery.** OpenCode's customization lookup points at a SwarmOS control directory. Provider data stays in its normal location.
- **Files and shell.** Same Bubblewrap executor as Claude Code: detached snapshot at `/workspace`, read-only `/usr`, private temporary storage, clean environment, no network, and read-only source for planners and reviewers.
- **Limits.** Two-minute tool deadlines, per-process resource limits, and 256 MiB of temporary storage per call. Cancelling a run stops detached tool children.

## Telemetry

SwarmOS does not open OpenCode's session database, which holds all of your sessions. Instead it creates the run's own session on the private server before the first turn, then asks that server for that session's messages after the last turn with `opencode api --server <private url> session.messages --param sessionID=<id>`. It never lists sessions, and it sends nothing anywhere. The request has its own 10-second limit, so it can add up to 10 seconds to the end of a run, and an export over 4 MiB is discarded.

- **Reported:** input tokens, output tokens, cache-read tokens as cached, reasoning tokens, tool calls and repeated identical shell commands, and the model id.
- **Unavailable:** effort.

A message that belongs to any other session makes the whole export `unavailable`. The session stays in your OpenCode history in its normal data store, as an interactive session would. SwarmOS never copies, moves or deletes it. The `session.create` and `session.messages` operation names and the message format still need a check against a real 2.1.2 run.

A value the harness does not report stays `unavailable` in `swarm issue status`, `swarm pr status` and the Draft PR evidence. A missing, ambiguous, linked, oversized or malformed record makes every value from it `unavailable` and never fails the run. These figures never affect approval, review, validation or import.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `requires at least 2.1.2` | Upgrade to OpenCode 2. Version 1 configuration is not supported. |
| `OpenCode exposed an unapproved MCP server` | A global MCP server is leaking into the private configuration. Check your OpenCode settings. |
| `OpenCode did not bind a private loopback server` | Make sure loopback networking is available to your user. |

## Qualification

The installed CLI passes an offline model and tool cycle, rejects a forged host-shell call, and stops detached tool children on cancellation. A live authenticated provider run is still pending. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).

Agent budgets default to 60 minutes and can be configured globally or per role in [approved execution policy](../project.md#agent-run-budgets). Interactive native sessions count working time across turns, exclude waits for coordinator input, and have a four-hour wall cap. A warning appears before the deadline. On timeout, native Codex and harness-tools Bubblewrap preserve a bounded recovery patch for [manual inspection](../developer.md#recovering-from-an-interrupted-run). Interactive OCI counts wall time without idle exclusion, and OCI has no timeout patch export. Individual tool-call limits remain separate.
