# Developer, Team & Worker Guide

This guide covers day-to-day SwarmOS workflows—whether you are an individual developer, an open-source contributor donating agent compute to a project's ready queue, or an engineering team running internal worker pools across private GitHub repositories. Pass `--backend github` when working against GitHub Issues, or `--backend local` when running an offline local fixture.

---

## 1. Everyday Task Flow

```bash
# Inspect the resolved agents, gate, and base commit without claiming or running processes
swarm work 42 --dry-run --backend github

# Reserve an approved task and provision .swarm/worktrees/issue-42
swarm claim 42 --backend github

# Run (or resume) planning, review gate, implementation, and validation
swarm work 42 --backend github

# Run validation checks directly or submit the validated branch as a Draft PR
swarm validate 42 --backend github
swarm submit 42 --backend github
```

Helpful CLI conventions:
- **Picking up the next task**: Use `swarm pull` to reserve the next eligible `swarm:ready` issue, or `swarm work --next` to claim and execute it immediately. Add `--all` with `--next` (or `swarm pull --all`) to scan across all repositories registered in `~/.config/swarm/repos.yaml`.
- **Interactive & harness-driven staging (`swarm stage`)**: Working inside an interactive coding harness without spawning nested subprocesses? Run `swarm claim <issue>`, write `.swarm/active/PLAN.md` and `TEST_PLAN.md`, and stage each phase directly with `swarm stage <issue> --phase plan --harness <name> --model <name>` and `swarm stage <issue> --phase implement --harness <name> --model <name>`.
- **Token auth without `gh` CLI**: Set `SWARM_GITHUB_TOKEN` (or `GH_TOKEN` / `GITHUB_TOKEN`) to authenticate directly with GitHub REST and Git HTTPS remotes on headless machines without installing `gh`.
- **Headless vs. interactive**: `swarm work` runs non-interactively (`--headless=true`) by default. Pass `--headless=false` to follow the agent's work and add instructions between turns.
- **Structured JSON output**: Add `--json` to any lifecycle command (`issue`, `plan`, `claim`, `pull`, `work`, `stage`, `review`, `validate`, `submit`, `status`, `renew`, `release`, or `node run`) for scripts and automation.

### Choose how you work

Start interactively when you want to stay close to the changes. Move repeatable tasks to unattended workers once the tools and permissions are set up.

| Working style | How to use it |
| :--- | :--- |
| **Pair with an agent** | Run `swarm work 42 --headless=false`. Native Codex streams its work, then accepts a follow-up instruction; press Enter to finish the phase. |
| **Stay in your current agent session** | Use `swarm claim` or `swarm pull`, work in the assigned worktree, and record each phase with `swarm stage`. Your existing session keeps its own permissions. |
| **Let a worker pick up tasks** | Run `swarm work --next` for one task or `swarm node run --watch` for a continuing queue. Both run headlessly; configure the harness to complete allowed work without waiting for keyboard input. |

Native Codex uses the same bounded permissions for pairing and unattended work. Allowed commands run without approval prompts; requests for broader access are denied. Each turn uses an ephemeral session, with task files carrying progress forward. For an existing agent session or `trusted-local` setup, keep the harness's approval prompts enabled when pairing. Reserve full-access or “YOLO” settings for disposable workers with independently enforced isolation; CI alone does not provide that boundary.

You can still pause automated work for a maintainer's plan review with `--gate human`. That checkpoint is separate from the harness's per-command permission prompts.

### Choose where agents run

SwarmOS uses your agent's normal login and security controls. On **Linux**, Codex uses its native permission profiles and sandbox. Claude Code, OpenCode 2, and OhMyPi keep their normal provider sessions and use a restricted tool executor for workspace editing and commands. Install your harness, sign in normally, then run `swarm doctor`. The tool executor also needs Bubblewrap, Python 3, Bash, and `prlimit` installed under `/usr`.

`doctor` marks the selected agents and their roles. An unavailable selected agent fails the check; `OPTIONAL` identifies unused harnesses that still need setup or qualification. Binary installation is listed separately from execution support. Adapters check minimum versions, supported interfaces, and actual filesystem/network restrictions. Newer stable versions can pass these checks without an exact-version allowlist. See the [qualification guide](native-qualification.md) for tested versions and the dogfood workflow.

| Execution mode | Use it for |
| :--- | :--- |
| **`isolated` (default)** | The selected harness's verified tool boundary, with task files separated from live Git state and host credentials. Set `execution.backend: oci` for the optional container backend. |
| **`trusted-local`** | Repositories and tools you trust to run under your OS account. Select it explicitly with `SWARM_EXECUTION_MODE=trusted-local` where project policy permits. |

> [!NOTE]
> Model connectivity stays in the authenticated harness. Isolated tools can use the task snapshot, private scratch space, and system runtime files; tool network access is disabled. Install build tools and dependencies locally. The [capability table](ARCHITECTURE.md#tool-capabilities) describes editing, shell access, and extension restrictions. Native macOS and Windows execution still need qualification; Windows users can use a qualified Linux setup in WSL2. Authenticated OCI workers are a separate roadmap item.

Run `swarm doctor` and `swarm work 42 --dry-run` to check the setup before claiming work. See [project configuration](project.md#shared-defaults-and-personal-choices) for shared defaults and agent presets, and [execution modes](../SECURITY.md#execution-modes) for runtime setup. `--backend local` selects local coordination storage; execution mode still controls how workers run.

---

## Agent integration runbook

Every usable coding tool is configured under `agents:` in `.swarm/config.yaml`. The configuration key is the policy and audit name, while `harness` selects one of SwarmOS's four built-in CLI drivers (`claude`, `codex`, `opencode`, or `omp`; defaults to the key name when omitted).

The following are the `trusted-local` CLI drivers. Isolated adapters use the same bounded tools for both working styles; pairing streams the response and accepts follow-up instructions.

| Harness Driver | Headless Invocation | Interactive Invocation |
| :--- | :--- | :--- |
| **[Claude Code](https://code.claude.com/docs/en/setup)** (`claude`, checked with `2.1.308`) | `claude -p --append-system-prompt-file <prompt> <task>` | `claude --append-system-prompt-file <prompt> <task>` |
| **[OhMyPi](https://github.com/can1357/oh-my-pi#install)** (`omp`, checked with `18.12.5`) | `omp -p --no-pty --append-system-prompt <prompt> <task>` | `omp --append-system-prompt <prompt> <task>` |
| **[OpenAI Codex](https://learn.chatgpt.com/docs/codex/cli)** (`codex`, native integration checked with `0.163.0`) | `codex exec <combined prompt>` | `codex <task>` |
| **[OpenCode 2](https://dev.opencode.ai/v2/docs/)** (`opencode`, V2; checked with `2.1.2`) | `opencode run --standalone <combined prompt>` | `opencode mini --standalone --prompt <task>` |

When an entry sets `model: <name>`, SwarmOS passes that exact choice to the harness. With no model set, the harness selects its default; SwarmOS has no built-in model-name mapping. Isolated runs use controlled execution settings, so choose a project model here when you need a consistent selection across workers. For offline tests and local dry runs (`--backend local`), you can also configure the deterministic built-in `mock` runner with explicit `trusted-local` execution.

### Grok Bot and OpenAI Dot

Want to work through an always-on conversational teammate in addition to local CLI drivers?
- **[Working with Grok Bot & SwarmOS](grokbot.md)** — Connect Grok Bot to GitHub for repository conversations and PR reviews, run `swarm` on Grok Bot's cloud computer, or configure an xAI Grok model via `opencode`.
- **[Working with OpenAI Dot, Codex & SwarmOS](openai-dot.md)** — Add the repositories you want to swarm on to a Codex Cloud environment or connected dev machine and let your Dot coordinate approved tasks.

---

## Reviewer isolation and credentials

During an automated agent review (`--gate agent`), SwarmOS runs the reviewer against a separate read-only snapshot of the worktree where the only imported output is `.swarm/active/REVIEW.md`. In `isolated` mode, the selected native or OCI backend enforces this boundary. In `trusted-local` mode, choose a reviewer sandbox:

- **macOS**: Uses `sandbox-exec` to block writes outside the temporary scratch space and review output channel.
- **Linux**: Uses `bwrap` (`bubblewrap`) with user, IPC, PID, UTS, and default network namespace isolation. On AppArmor-enabled Linux distributions, ensure the distribution's `bwrap` AppArmor profile is installed (Ubuntu 26.04 includes this out of the box; on Ubuntu 24.04 see [LP #2069526](https://bugs.launchpad.net/ubuntu/+source/apparmor/+bug/2069526) or use an OCI container runtime).
- **OCI Container (`sandbox: container`)**: Supported on Linux, macOS, and Windows (where no native host sandbox exists) using `docker` or `podman` with a read-only root filesystem, dropped capabilities, and explicit scratch/output mounts.
- **Reviewer network policy**: Reviewer network access is disabled by default; set `review_network: true` on a reviewer agent entry when it needs outbound API access to reach its model provider.

In `trusted-local` mode, agent subprocesses start with a clean temporary `HOME` and receive only the model-authentication environment variables explicitly listed in `agents.<name>.env` (publishing/SSH variables such as `GH_*`, `GITHUB_*`, `GIT_*`, and `SSH_*` remain with the `swarm` coordinator process). Configure credentials through those allowed variables; your usual home-directory login and settings are not automatically loaded. Planner, developer, and tester roles execute your project's build and test tools inside their assigned worktree as host processes under your user account. Isolated workers receive no host credential variables.

---

## 2. Checkpoints, Handoffs & Recovery

Every claim records a unique fencing token (`claim_id`), approved base SHA, branch name, and checkpoint commit SHA. The local claim token is stored in your user config directory (`swarm/claims/<git-common-dir-hash>/<issue>.json`) so same-machine commands resume cleanly without colliding.

### Handing Off a Plan or Review to Another Teammate
One teammate or node can complete the planning phase, checkpoint the branch and artifacts to Git, and hand the task off for another worker to review or implement:

```bash
# Teammate A generates and checkpoints PLAN.md + TEST_PLAN.md, then releases into handoff mode
swarm work 42 --phase plan --handoff --backend github

# Teammate B (or a worker node) picks up the handed-off task and continues from the verified checkpoint
swarm work --next --all --backend github
```

### Recovering from an Interrupted Run
If an agent exits early, a lease expires, or a validation check fails:
1. Run `swarm status <issue> --backend github` to inspect the current lease, phase, and published artifacts.
2. Fix the code, test, or agent configuration inside `.swarm/worktrees/issue-<N>`.
3. Re-run `swarm work <issue> --backend github`—if your claim token is still active, SwarmOS resumes in place; if the lease expired, it acquires a fresh attempt on the verified checkpoint branch.
4. Run `swarm validate <issue>` and `swarm submit <issue>` when all checks pass.

---

## 3. Multi-Repository Registry & Worker Pools

Register the repositories you want to work on so `swarm pull --all`, `swarm work --next --all`, and `swarm node run --all` can coordinate across them:

```bash
# Register the current checkout or clone a remote GitHub repository
swarm repo add --backend github
swarm repo add OWNER/REPO --clone --backend github
swarm repo list

# Initialize a worker node profile (~/.config/swarm/node.yaml)
swarm node init

# Drain up to 4 ready tasks across registered repositories with 2 concurrent workers
swarm node run --all --concurrency 2 --max-jobs 4 --auto-submit --backend github
```

By default, `swarm node run` drains the currently available ready queue and exits cleanly. Add `--watch` (and optional `--poll-interval 30s`) when you want the worker pool to keep polling for newly approved tasks.

---

## Related Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](ADMIN_GUIDE.md)** — Roadmap decomposition (`swarm plan`), backlog triage, and Draft PR auditing.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](GITHUB.md)** — Branch rulesets, `SWARM_GITHUB_TOKEN`, and collaborator allowlists.
- **[Project & Maintainer Setup (`docs/project.md`)](project.md)** — Initialize `.swarm/`, configure agents and checks, and approve tasks.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS state refs, fencing, and checkpoint mechanics.
- **[Security Policy (`SECURITY.md`)](../SECURITY.md)** — Trust boundaries, reviewer isolation, and release verification.
