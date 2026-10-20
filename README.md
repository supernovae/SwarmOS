# SwarmOS

SwarmOS turns an approved GitHub Issue WorkUnit into a plan-first, worktree-based change and an optional Draft PR. It is for repositories that deliberately configure the agents, validation commands, and approval policy they trust.

> **Documentation target:** `main` / unreleased work. The latest published release is **v0.1.0** and may not provide the commands, Git-CAS state model, sandbox requirements, or flags described here. Check `swarm version` and release notes before using a release binary with this guide.

## What is authoritative

- A WorkUnit is parsed from an issue and approved as an immutable digest with a policy digest and base SHA.
- Git compare-and-swap state refs are the coordination authority. Issue comments are human-readable audit projections, not authority.
- A claim has a unique fencing token. State, artifact, checkpoint, and submission mutations verify the live claim, revision, and expiry.
- Each task starts from the approved base in `.swarm/worktrees/issue-<N>` on `swarm/issue-<N>-<slug>`.
- Validation reads the policy at the approved base; a candidate cannot redefine its own checks or protected paths.

This is not a security boundary around an arbitrary agent. A worktree separates Git working directories; it does not itself restrict filesystem access, network access, or processes. Agent children start with a temporary credential-free home and receive only configured environment entries; reviewer execution additionally requires a supported sandbox and imports only its bounded review artifact. See [Security](SECURITY.md) and [architecture](docs/ARCHITECTURE.md).

## Lifecycle

```text
Draft WorkUnit → approve immutable revision → claim → plan + test plan
                                              → required gate → implement → validate → submit Draft PR
```

`workflow` YAML supplies the role prompt recipe and context. It is **not** an executable graph: the fixed engine runs plan, optional review, implementation, and validation. `submit` is deliberately separate.

A `human` gate pauses after the plan. An `agent` gate accepts only an exact reviewer verdict bound to the current WorkUnit, plan, and test-plan hashes. Editing any bound input requires a new approval/review. A CLI `--gate` may make policy stricter; it cannot weaken a repository/task gate.

## Quick start

```bash
# Set SWARM_VERSION to an existing compatible release tag or full commit SHA.
: "${SWARM_VERSION:?Set a compatible immutable SwarmOS version}"
swarm init --sync-labels --backend github --swarm-version "$SWARM_VERSION"
swarm issue create --title "Add version output" \
  --objective "Print build information" \
  --constraint "Use the standard library" \
  --acceptance "swarm version --json emits JSON" \
  --workflow feature --gate agent

# Replace 42 below with the issue number printed by issue create.
# New issues are drafts. A permitted actor approves this exact revision.
swarm issue approve 42

# Inspect selection and effective policy without claiming or running.
swarm work 42 --dry-run

# Work is headless by default; pass --headless=false for an interactive harness.
swarm work 42
swarm submit 42
```

For an offline fixture, say so explicitly: pass `--backend local`. SwarmOS does not silently fall back from a GitHub repository to local tracking.

## Agent configuration

Only names in `.swarm/config.yaml` under `agents:` are selectable. Agent resolution is role-specific CLI override → generic `--agent` → `roles.<role>` → `default_agent`. Before a claim, SwarmOS verifies that the resolved configured agent is allowed for that role and available. `mock` is local/test-only.

Supported harness drivers are Claude Code (`claude`), Codex (`codex`), OpenCode (`opencode`), and OhMyPi (`omp`). A configured agent name is the policy/audit identity; its `harness` selects one of those CLI argument contracts. `model` is applied only through a supported harness. `grok` and `dot` have no supported native harness: explicit configuration must select a supported driver and independently prove that its binary accepts that driver's interface. They are not vendor-guaranteed integrations. See the shared [integration runbook](docs/developer.md#agent-integration-runbook).

## Interfaces for people and automation

- Lifecycle commands support structured `--json` results (`issue create|approve|lint|decompose`, `claim`, `pull`, `work`, `review`, `validate`, `submit`, `status`, `renew`, `release`, and `node run`). `work --json` requires headless execution; use these result objects rather than parsing presentation text. `work --dry-run` emits `kind: "work-dry-run"`.
- `swarm node run --watch` is intended for continuous polling; do not assume it is present in v0.1.0. A normal node invocation drains available work and exits.
- `claim`, `pull`, and `work` are intentionally distinct: reserve a named task, select/reserve a task, and execute/resume a task.

## Guides

- [Repository setup and approval](docs/project.md)
- [Developer operations, recovery, and integrations](docs/developer.md)
- [Prompt recipes and plan gates](docs/WORKFLOWS.md)
- [Git-CAS architecture and checkpoints](docs/ARCHITECTURE.md)
- [Grok wrapper note](docs/grokbot.md) and [Dot/Codex wrapper note](docs/openai-dot.md)
- [Security boundaries](SECURITY.md)
- [Optional future coordinator requirements](todo.md)
