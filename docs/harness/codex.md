# OpenAI Codex (`codex`)

Codex is SwarmOS's most fully qualified harness. In isolated mode, its own permission profiles and Linux sandbox enforce the tool boundary, so it needs no extra executor. It uses your normal Codex login and session renewal.

## Setup

1. Install Codex CLI 0.163.0 or newer.
2. Sign in with `codex login`. On a headless machine, use `codex login --device-auth`.
3. Confirm with `codex login status`, then run `swarm repo doctor`.

```yaml
agents:
  codex:
    binary: codex
    model: your-preferred-model   # optional
    effort: high                  # optional: minimal, low, medium or high
```

`effort` sets Codex's reasoning effort with `-c model_reasoning_effort="<effort>"`. SwarmOS accepts only the values above and records the effort in each run's provenance. For a different effort per role, add one entry per role (for example `codex-reviewer` with `harness: codex`) and map it under `roles:`.

[OpenAI Dot](../openai-dot.md) setups can define a `dot` entry with `harness: codex` so Dot-driven runs show up under their own audit name.

## How SwarmOS runs Codex

| Mode | Invocation |
| --- | --- |
| Isolated, unattended | `codex exec --ignore-user-config --ignore-rules --ephemeral --skip-git-repo-check -C <snapshot>` with a SwarmOS permission profile, plus `--model <model>` and `-c model_reasoning_effort="<effort>"` when configured |
| Isolated, interactive | The same `codex exec` settings. After each turn, SwarmOS asks for a follow-up instruction; press Enter to finish the phase. |
| Isolated validation | `codex sandbox -P swarm` runs approved-base commands in a disposable snapshot without provider credentials |
| `trusted-local` | `codex exec "<prompt>"` unattended, or `codex "<task>"` interactively |

Each turn is ephemeral. Task files carry progress between turns and phases, so an interrupted turn restarts cleanly from the checkpoint.

## Boundary

- **Files.** Tools see a detached task snapshot, private scratch space, and Codex's minimal system runtime files. Live Git metadata, coordinator state, host credential stores, and autoloaded `.codex` and `.agents` directories are left out.
- **Shell.** Commands run in Codex's Linux sandbox with a clean environment. Provider and publishing tokens are filtered out, and tool networking is disabled.
- **Permissions.** Pairing and unattended runs use the same profile. Allowed commands run without prompts, and requests to escalate are denied.
- **Disabled.** User configuration and rules, MCP, plugins, hooks, browser tools, and subagents.
- **Checks before work.** SwarmOS tests outside-file, symlink, write, and loopback canaries, and confirms the required `codex exec` flags exist.

The Codex process and its internal tool dispatcher run on the host and are trusted. The boundary protects against agent-directed tools, not a compromised Codex binary. Each unattended phase defaults to a 60-minute agent budget with bounded imports, but no CPU, memory, or disk quotas. Use the OCI backend if you need those.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `codex login is unavailable` | Run `codex login`, then `swarm repo doctor`. |
| `requires at least 0.163.0` or `unsupported version` | Install a stable Codex release at or above the minimum. |
| A missing `exec` flag | The installed release lacks a required interface. Update Codex. |
| A canary or sandbox failure | Run on a Linux host where Codex's sandbox and loopback networking work. WSL2 is supported. |

`doctor` reports a login it finds through `codex login status`, but it does not make a model request. If Codex asks you to sign in again during a WorkUnit, sign in and resume with `swarm issue work <issue>`.

## Qualification

Qualified on Linux/amd64 with an authenticated WorkUnit covering all four roles, interactive planning, a human gate, unattended development and testing, native validation, and submission. It has also run through hosted GitHub issue, PR, and fork PR revision flows. The test commands are in the [qualification guide](../native-qualification.md#codex).

Codex's [permission profiles](https://learn.chatgpt.com/docs/permissions) are still beta, so review each new release before raising the minimum version.

Agent budgets default to 60 minutes and can be configured globally or per role in [approved execution policy](../project.md#agent-run-budgets). Interactive native sessions count working time across turns, exclude waits for coordinator input, and have a four-hour wall cap. A warning appears before the deadline. On timeout, native Codex and harness-tools Bubblewrap preserve a bounded recovery patch for [manual inspection](../developer.md#recovering-from-an-interrupted-run). Interactive OCI counts wall time without idle exclusion, and OCI has no timeout patch export. Individual tool-call limits remain separate.
