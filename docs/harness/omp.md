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
| Isolated | `omp --mode rpc --no-ui --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-pty --config <swarm config>` |
| `trusted-local` | `omp -p --no-pty --append-system-prompt <system prompt> "<task>"` unattended, or the same without `-p --no-pty` interactively |

In RPC mode, SwarmOS answers OhMyPi's host tool calls itself, routing each one through the Bubblewrap executor. It handles tool cancellation too.

## Boundary

Linux native isolated Go commands and worker checks use `GOMAXPROCS=2` and `GOFLAGS=-buildvcs=false -p=2`; see the [shared concurrency defaults](README.md#go-concurrency).

- **Tool catalog.** Before each turn, SwarmOS checks that OhMyPi's effective tool list contains exactly the approved task tools.
- **Files and shell.** Same Bubblewrap executor as Claude Code: detached snapshot at `/workspace`, read-only `/usr`, private temporary storage, clean environment, no network, and read-only source for planners and reviewers.
- **Limits.** Two-minute tool deadlines, per-process resource limits, and 256 MiB of temporary storage per call.

## Telemetry

OhMyPi saves the run's session as `~/.omp/agent/sessions/<cwd>/<timestamp>_<session id>.jsonl`, where `<cwd>` is the run's private control directory with each `/` replaced by `-`. After the run, SwarmOS lists only that directory and reads its one session file, and only if the file's `session` header names that control directory and the id in the file name. It sends nothing anywhere.

- **Reported:** input tokens (without cache reads), output tokens, `cacheRead` tokens as cached, reasoning tokens, tool calls and repeated identical shell commands, and the model.
- **The model is OhMyPi's configured model id**, the one OhMyPi resolved from its settings or `--model`, not the model name the provider returned in its response.
- **Unavailable:** effort.

The session stays in your OhMyPi history under `~/.omp/agent/sessions`, as an interactive session would. SwarmOS never copies, moves or deletes it. This layout was checked against OhMyPi 18.12.6 in both `--mode json` and `--mode rpc`.

A value the harness does not report stays `unavailable` in `swarm issue status`, `swarm pr status` and the Draft PR evidence. A missing, ambiguous, linked, oversized or malformed record makes every value from it `unavailable` and never fails the run. These figures never affect approval, review, validation or import.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `requires at least 18.12.5` | Update OhMyPi. |
| `lacks required interface` | The installed release is missing an RPC or isolation flag. Update OhMyPi. |
| A tool executor check fails | Install `bwrap`, Python 3, Bash, and `prlimit` under `/usr`, and make sure unprivileged user namespaces are enabled. |

## Qualification

The real RPC interface exposes exactly the approved task tools. An authenticated interactive tool task has passed with synthetic host-file denial and workspace editing and testing. A full authenticated WorkUnit is next. The test commands are in the [qualification guide](../native-qualification.md#claude-code-opencode-2-and-ohmypi).

Agent budgets default to 60 minutes and can be configured globally or per role in [approved execution policy](../project.md#agent-run-budgets). Interactive native sessions count working time across turns, exclude waits for coordinator input, and have a four-hour wall cap. A warning appears before the deadline. On timeout, native Codex and harness-tools Bubblewrap preserve a bounded recovery patch for [manual inspection](../developer.md#recovering-from-an-interrupted-run). Interactive OCI counts wall time without idle exclusion, and OCI has no timeout patch export. Individual tool-call limits remain separate.
