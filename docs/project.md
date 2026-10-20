# Project & Maintainer Setup Guide

This guide walks repository maintainers through initializing SwarmOS, configuring `.swarm/config.yaml` and validation rules, authoring and approving `WorkUnit` issues, and setting up GitHub repository protections.

---

## 1. Initialize and Commit the Harness

Download the latest `swarm` release from [**GitHub Releases**](https://github.com/supernovae/SwarmOS/releases/latest), then run `swarm init` inside your repository checkout and pin the release tag you want your consumer CI workflow to install:

```bash
swarm init --sync-labels --backend github --swarm-version v0.3.0
swarm doctor --backend github
```

`swarm init` detects your project stack (`Go`, `Rust`, `Python`, or `TypeScript/JavaScript`) and scaffolds:

| Path | Role |
| :--- | :--- |
| `.swarm/config.yaml` | Default agent, `plan_mode` (`auto\|agent\|interactive`), plan review gate, lease TTL/renewal limits, role routing, collaborator allowlists, and allowed agent harnesses. |
| `.swarm/goals.md`, `SKILLS.md`, `rules/`, `skills/` | Project mission, safety invariants, agent factory staging guide, and reusable rules/skills compiled into `SYSTEM_PROMPT.md`. |
| `.swarm/project.md`, `architecture.md`, `conventions.md` | Project context, build/test commands, and conventions compiled into agent prompts. |
| `.swarm/workflows/{feature,bugfix,refactor}.yaml` | Role prompt recipes for feature delivery, bug fixes, and refactors. |
| `.swarm/prompts/{planner,reviewer,developer,tester}.md` | Role contracts defining what each phase reads and produces. |
| `.swarm/contracts/{workunit,artifact}.schema.json` | JSON Schemas (Draft 2020-12) for validating issues and artifacts. |
| `.swarm/validation/required-checks.yaml` | Required check commands and protected-path globs enforced before submission. |
| `.github/ISSUE_TEMPLATE/{swarm-workunit,swarm-feature,swarm-bugfix,swarm-refactor,config}.yml` | Preset GitHub Issue Forms for creating structured WorkUnits from the GitHub UI (customizable via `--template-dir`). |
| `.github/workflows/swarm-validate.yml` | Consumer CI workflow running your repository checks against the trusted base commit. |
| `.claude/commands/swarm-*.md` & `.opencode/command/swarm-*.md` | `/swarm-plan`, `/swarm-review`, `/swarm-implement`, `/swarm-validate`, and `/swarm-project-plan` slash commands. |

> [!TIP]
> Passing `--swarm-version` ensures the generated `.github/workflows/swarm-validate.yml` installs that exact immutable SwarmOS version. Commit the generated `.swarm/`, `.github/`, `.claude/`, and `.opencode/` files to your default branch before approving work.

---

## Consumer CI and release administration

When `--swarm-version` is provided, `swarm init` generates `.github/workflows/swarm-validate.yml` configured for your repository's default branch and language toolchain (`Go`, `Node 24`, `Python 3.13` with `pytest`/`ruff`, or `Rust` with `clippy`), then runs `swarm validate --ci --base "$BASE_SHA"` against the pull request or push base SHA.

For repositories that mirror SwarmOS's own release and security setup:
- **Release workflow (`release.yml`)**: Runs the lint, vulnerability, and Linux/macOS/Windows test matrix for the tagged commit, verifies the semantic version tag is reachable from the default branch, and publishes through the `release` GitHub Environment. Configure required reviewers and tag protection rules on that environment in GitHub Settings.
- **Dependency Review (`security.yml`)**: Requires GitHub's [dependency graph](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/enable-dependency-graph) to be enabled under **Settings → Advanced Security** (along with Dependabot alerts).

---

## 2. Configure Approved Agents (`.swarm/config.yaml`)

```yaml
version: 1
default_agent: claude
default_gate: agent

roles:
  planner: claude
  reviewer: omp
  developer: codex
  tester: opencode

lease:
  duration: 4h
  renewable: true
  max_renewals: 3

security:
  require_ready_label: true
  ready_label: swarm:ready

agents:
  claude:
    binary: claude
    roles: [planner, developer]
  omp:
    binary: omp
    roles: [reviewer]
    sandbox: sandbox-exec   # or bwrap on Linux, container with OCI runtime
  codex:
    binary: codex
    roles: [developer]
  opencode:
    binary: opencode
    roles: [tester]
```

How agent configuration works:
- **Explicit allowlist**: Only entries under `agents:` can be selected. Each entry can configure `binary`, `harness` (`claude`, `codex`, `omp`, or `opencode`; defaults to the entry name), `args`, `model`, allowed `roles`, permitted `env` variables, and reviewer sandbox settings.
- **Precedence**: Role-specific CLI flag (`--planner-agent`, `--reviewer-agent`, `--developer-agent`, `--tester-agent`) $\rightarrow$ generic `--agent` $\rightarrow$ `roles.<role>` $\rightarrow$ `default_agent`.
- **Maintainer-controlled approvals**: When `security.require_ready_label` is `true`, an issue must be approved by a user with repository write/maintain/admin permissions via `swarm issue approve <issue>` (or `swarm issue create --ready`) before any worker can claim it.

---

## 3. Create, Lint, and Approve WorkUnit Issues

You can create structured WorkUnits from the GitHub Issue Form, fenced YAML blocks, external `.swarm/workunits/<id>.yaml` references, or the CLI:

```bash
# Create a draft WorkUnit issue on GitHub (add --ready to approve in one step)
swarm issue create --backend github \
  --title "Add retry policy" \
  --objective "Retry transient HTTP failures" \
  --constraint "Preserve the public client API" \
  --acceptance "Retries 429 responses within the configured limit" \
  --workflow feature --gate human

# Validate the issue schema and approve its revision for execution
swarm issue lint 42 --backend github
swarm issue approve 42 --backend github
```

Approving an issue records the canonical WorkUnit digest, `.swarm/` policy digest, and trusted base SHA in Git state and applies the `swarm:ready` label. If an issue's scope or `.swarm/` policy changes later, run `swarm issue approve <issue>` again to approve the updated revision.

To split a larger epic into sequential child issues with `depends_on` links, run:

```bash
swarm issue decompose 42 --sequential --backend github
```

---

## Validation policy and protected paths

`.swarm/validation/required-checks.yaml` defines the checks and protected paths that every task must satisfy:

```yaml
required_checks:
  - name: build
    command: go build ./...
    timeout: 5m
  - name: test
    command: go test ./...
    timeout: 10m
  - name: lint
    command: go vet ./...
    timeout: 5m
protected_paths:
  - .github/workflows/**
  - .swarm/config.yaml
  - .swarm/validation/**
  - .swarm/contracts/**
  - .swarm/prompts/**
  - .swarm/workflows/**
```

During `swarm validate` and `swarm submit`, SwarmOS reads `required-checks.yaml` from the **approved base commit** rather than the candidate worktree, so a task cannot pass validation by weakening its own check commands or protected-path rules. When a maintainer intentionally authors a task that updates `.swarm/` or `.github/workflows/`, set `allow_harness_changes: true` in that issue's WorkUnit before approving it.

---

## 4. Plan Review Gates (`none | agent | human`)

- **`none`**: Runs the planner to generate `PLAN.md` and `TEST_PLAN.md`, then proceeds directly to implementation and validation.
- **`agent`**: Runs your configured reviewer agent inside an isolated read-only snapshot. Implementation starts only when `.swarm/active/REVIEW.md` begins with `VERDICT: APPROVED` (bound to the current WorkUnit, plan, and test-plan digests).
- **`human`**: Pauses after publishing the plan so a maintainer can inspect the design and run `swarm review <issue> --approve` or `--reject`.

A CLI `--gate` flag can always make review stricter (for example, upgrading `none` to `agent` or `human`), while preserving the minimum gate required by the issue and repository policy.

---

## Related Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](ADMIN_GUIDE.md)** — Daily maintainer playbook for `swarm plan`, triage labels, and Draft PR provenance review.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](GITHUB.md)** — Configure repository rulesets, `SWARM_GITHUB_TOKEN`, and role-separated collaborator accounts.
- **[Developer & Worker Guide (`docs/developer.md`)](developer.md)** — Everyday CLI workflows, interactive `swarm stage`, agent harness table, reviewer isolation, checkpoints, and recovery.
- **[Working with Grok Bot (`docs/grokbot.md`)](grokbot.md)** — Connect Grok Bot to GitHub and coordinate SwarmOS tasks.
- **[Working with OpenAI Dot & Codex (`docs/openai-dot.md`)](openai-dot.md)** — Configure Codex Cloud environments and remote repositories for OpenAI Dots.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS coordination, fencing, and worktree lifecycle.
