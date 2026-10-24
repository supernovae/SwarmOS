# OhMyPi (`omp`)

OhMyPi keeps its existing provider login. In isolated mode, SwarmOS starts it in RPC mode with every built-in tool, extension, and rule turned off, and registers SwarmOS's tools as host tools. Those tools run inside Bubblewrap.

## Setup

1. Install OhMyPi 18.12.5 or newer.
2. Sign in to your model provider through OhMyPi's normal login flow.
3. Install Bubblewrap (`bwrap`), Python 3, Bash, and `prlimit` under `/usr`.
4. Run `swarm repo doctor`.

```yaml
agents:
  omp:
    binary: omp
    model: your-preferred-model   # optional
```

## How SwarmOS runs OhMyPi

| Mode | Invocation |
| --- | --- |
| Isolated | `omp --mode rpc --no-ui --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-pty --no-session --config <swarm config>` |
| `trusted-local` | `omp -p --no-pty --append-system-prompt <system prompt> "<task>"` unattended, or the same without `-p --no-pty` interactively |

In RPC mode, SwarmOS answers OhMyPi's host tool calls itself, routing each one through the Bubblewrap executor. It handles tool cancellation too.

## Boundary

- **Tool catalog.** Before each turn, SwarmOS checks that OhMyPi's effective tool list contains exactly the approved task tools.
- **Files and shell.** Same Bubblewrap executor as Claude Code: detached snapshot at `/workspace`, read-only `/usr`, private temporary storage, clean environment, no network, and read-only source for planners and reviewers.
- **Limits.** Two-minute tool deadlines, per-process resource limits, and 256 MiB of temporary storage per call.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `requires at least 18.12.5` | Update OhMyPi. |
| `lacks required interface` | The installed release is missing an RPC or isolation flag. Update OhMyPi. |
| A tool executor check fails | Install `bwrap`, Python 3, Bash, and `prlimit` under `/usr`, and make sure unprivileged user namespaces are enabled. |

## Qualification

The real RPC interface exposes exactly the approved task tools. An authenticated interactive tool task has passed with synthetic host-file denial and workspace editing and testing. A full authenticated WorkUnit is next. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).
