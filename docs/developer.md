# Developer operations and agent integration runbook

This guide documents the unreleased Git-CAS lifecycle. Use an explicit backend: `--backend github` for GitHub tracking or `--backend local` for a deliberate offline fixture. There is no implicit local fallback.

## Everyday task flow

```bash
# Reserve only; preflights the configured planner and persists this machine's claim token.
swarm claim 42

# Or execute an explicit approved issue. This resumes the persisted token when present.
swarm work 42 --headless

# Inspect without a claim, worktree, or agent process.
swarm work 42 --dry-run

# Validate then submit the current checkpoint as a Draft PR.
swarm validate 42
swarm submit 42
```

Use `swarm work --next` only to select a next eligible approved task. It is mutually exclusive with an explicit issue number. `--all` is meaningful only with `--next` for registered repositories. `claim`, `pull`, and `work` intentionally remain separate so reservation does not imply execution.

Use `--json` where a command exposes it for scripts. JSON is the machine interface; do not parse status tables or progress prose. `work --dry-run` yields the effective selection, including resolved policy/gate/agents, before any mutation.

`work` is headless by default; use `--headless=false` only for an interactive terminal session. The same structured JSON convention is available on task lifecycle, issue, status, lease, and node-run commands. `validate <issue>` uses the task's approved base SHA by default; `validate --ci` requires an explicit full immutable `--base` SHA and rejects `HEAD`.

## Agent integration runbook

Configure every usable harness in `.swarm/config.yaml`; only configured entries are allowed. The supported harness drivers are `claude`, `codex`, `opencode`, and `omp`. The configuration key is the policy/audit identity; its `harness` field selects the driver and CLI flags, while `binary`, optional arguments/model, allowed roles, permitted environment, and sandbox settings remain repository policy. A missing `harness` defaults to the configuration key, so nonstandard names must set it explicitly.

| Harness | Headless command shape | Interactive command shape |
| --- | --- | --- |
| [Claude Code](https://code.claude.com/docs/en/setup) 2.1.292 | `claude -p --append-system-prompt-file <prompt> <task>` | `claude --append-system-prompt-file <prompt> <task>` |
| [OhMyPi](https://github.com/can1357/oh-my-pi#install) 18.12.3 | `omp -p --no-pty --append-system-prompt <prompt> <task>` | `omp --append-system-prompt <prompt> <task>` |
| [Codex](https://learn.chatgpt.com/docs/codex/cli) 0.160.1 | `codex exec <combined prompt>` | `codex <task>` |
| [OpenCode 1](https://opencode.ai/docs/) 1.17.1 | `opencode run <combined prompt>` | `opencode --prompt <task>` |

The listed versions passed `--version` inside the macOS arm64 reviewer sandbox. This proves startup, not authenticated model execution; no paid model calls were made. OpenCode 2 and arbitrary newer versions require independent compatibility checks. An explicit configured `model` adds the driver's `--model` selection.

SwarmOS checks availability before a claim. It does not assert provider account access, model entitlement, billing, or vendor behavior beyond the configured executable invocation. `mock` is deterministic local/test-only and must be explicitly selected in an explicit local backend context.

### Grok and Dot wrappers

`grok` and `dot` have no native supported harness. A repository may use either name as an explicit policy/audit identity only when it selects a supported `harness` and independently verifies that the chosen binary accepts that harness's CLI contract. SwarmOS does not guarantee an official vendor distribution, version, or CLI compatibility. See [Grok wrapper](grokbot.md) and [Dot/Codex wrapper](openai-dot.md).

## Reviewer isolation and credentials

A reviewer is not made read-only by its prompt or by a worktree. The reviewer receives an immutable separate snapshot and an output-only `REVIEW.md` channel. The current implementation uses `sandbox-exec` on macOS, `bwrap` on Linux, or a configured OCI container runtime; there is no native Windows host sandbox, so Windows review requires the configured container path. An unavailable selected boundary fails closed. Linux `bwrap` uses user, IPC, PID, and UTS namespaces (and network isolation by default), so host user namespaces must be enabled. Container review is reviewer-only: it resolves the binary inside the image, runs with a read-only root, dropped capabilities/no-new-privileges, read-only snapshot, and explicit writable output/scratch mounts; on Unix it uses the host UID:GID, while Windows omits `--user` for bind-mount semantics. Docker on Linux requires accessible bind mounts. Reviewer network access is denied by default. `review_network: true` permits unrestricted egress for that configured reviewer so a cloud model CLI can operate; it is not a domain-filtered policy.

Agent children start with a temporary credential-free scratch home, not inherited host `HOME`, XDG, SSH, Git, or publishing-token state. Only exact configured model-authentication variables are forwarded through `AgentConfig.Env`; configuration rejects `GH_*`, `GITHUB_*`, `GIT_*`, `SSH_*`, `HOME`, and `XDG_*`. Do not relabel unrelated secrets into permitted names. The coordinator performs state and publication operations.

Host harness binaries are resolved to canonical paths. Because agent homes are masked, home-installed dependencies are unavailable to isolated review; use a system/global installation or a reviewed OCI image. Container/runtime preflight checks availability, but an OS/runtime restriction can still make review fail closed at launch.

This reviewer boundary does **not** sandbox planner, developer, or tester roles. Those configured host processes share the user UID and can directly read host files or credential stores despite the sanitized environment; they may also modify their assigned worktree and execute repository tools. Treat them as trusted host processes, not a general security boundary.

## Checkpoints, handoffs, and recovery

A claim records a unique attempt/claim ID, approved base SHA, branch, and checkpoint head. The claim token is persisted outside the worktree in the current user's configuration directory under `swarm/claims/<git-common-dir-hash>/<issue>.json`; same-machine execution is serialized, so a second command cannot resume the same token concurrently.

A checkpoint commits current work without history rewriting. For GitHub, the state revision and exact branch tip are published together with an atomic push fenced against both previous refs. Another checkout verifies the recorded branch and SHA before opening it. Do not copy a plan into a new unrelated checkout, recreate a deleted worktree with a reset branch, or rely on a Draft PR as a dependency's code transfer.

If an agent exits, a lease is lost, or validation fails:

1. Run `swarm status <issue>` to inspect live state and artifacts.
2. Fix configuration, sandbox, agent availability, or code in the verified worktree.
3. Resume with the persisted token if still live; otherwise reclaim a fresh attempt.
4. Re-plan/re-review if task, policy, plan, or test-plan digest changed.
5. Validate the exact current head before submitting.

## Workers

A normal `swarm node run` drains available work then exits. `--watch` is the continuous polling interface when present in your installed unreleased build; do not assume it is available in v0.1.0. The worker atomically reserves `--max-jobs` capacity before selection, reports failed selection/execution/submission as failures, and reports human-gated tasks as paused rather than completed. It stops child process trees on cancellation and only auto-submits after validation.

The `--max-jobs` admission limit is implemented. Wall-time/cost budgets and repository fairness remain planned optimization work. Agent output is bounded to 1 MiB; cost limits require provider usage data, and elapsed time is not a cost estimate.

Related: [setup](project.md) · [prompt and gate semantics](WORKFLOWS.md) · [security limits](../SECURITY.md).
