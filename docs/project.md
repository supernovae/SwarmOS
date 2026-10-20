# Repository setup and task approval

This guide targets unreleased `main`. The v0.1.0 release may not include the Git-CAS lifecycle or interfaces below.

## Initialize and commit the harness

```bash
SWARM_VERSION='your-compatible-release-tag-or-full-commit'
swarm init --sync-labels --backend github --swarm-version "$SWARM_VERSION"
swarm doctor
```

`--swarm-version` is an immutable release tag or full commit required to generate consumer CI. Without it, initialization deliberately omits the workflow and warns rather than emitting CI that installs a nonexistent or unpinned release. Pin a compatible released version or full SHA containing the consumer validator.

Commit generated `.swarm/`, issue form, and consumer CI configuration to the default branch. Consumer CI installs that pinned SwarmOS binary and runs the repository's approved checks; it must not assume the consumer contains `cmd/swarm` or Go source. It supplies the trusted PR or push base SHA to `swarm validate --ci --base`.

## Consumer CI and release administration

Generated consumer CI is emitted only with an explicit compatible immutable `--swarm-version`; it installs that pinned validator, detects the repository default branch rather than guessing `main`, and passes an explicit trusted base. It provisions the detected Go runtime, Node 24, Python 3.13 with `pytest`/`ruff`, or Rust `clippy` setup. A Python `pyproject.toml` repository may still require operator-managed dependency installation. If initialization cannot reliably identify a default branch, it omits CI with a warning.

SwarmOS's own release workflow runs the reusable lint, vulnerability, and Linux/macOS/Windows test matrix for the exact tag SHA, verifies that a semantic version tag is reachable from the repository default branch, then publishes through the `release` GitHub environment. Repository administrators must configure that environment's required reviewers and tag/branch protection: the workflow requests the environment but cannot itself enforce remote GitHub protection rules.

SwarmOS's `Dependency Review` PR check also requires GitHub's repository-level [dependency graph](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/enable-dependency-graph). Administrators must enable it under **Settings → Advanced Security** and keep Dependabot alerts enabled for ongoing vulnerability notifications. These are GitHub settings, not workflow YAML. A disabled graph causes a setup failure, not a clean dependency report; keep the check blocking rather than switching it to warn-only.

The tracked harness provides:

| Path | Role |
| --- | --- |
| `.swarm/config.yaml` | strict policy, configured agents, role routing, leases |
| `.swarm/workflows/*.yaml` | prompt recipes, not an executable graph |
| `.swarm/prompts/*.md` | planner, reviewer, developer, tester contracts |
| `.swarm/validation/required-checks.yaml` | candidate-facing copy of validation policy |
| `.swarm/project.md`, `architecture.md`, `conventions.md` | repository context compiled into prompts |

## Configure only approved agents

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
  codex:
    binary: codex
    roles: [developer]
  opencode:
    binary: opencode
    roles: [tester]
```

`agents` is an allowlist, not a discovery list. A configured entry may set `binary`, `harness`, `args`, `model`, `roles`, permitted `env`, and reviewer sandbox/network settings. The configured name is the policy/audit identity; `harness` selects the CLI argument contract and must be `claude`, `codex`, `omp`, or `opencode` (when omitted it defaults to the configured name). `model` is applied only through those supported harnesses. The exact resolved agent is checked before a claim. Resolution precedence is role CLI override, generic CLI agent, `roles`, then `default_agent`.

`require_ready_label` requires an authorized immutable task approval before execution. GitHub tracker authorization uses actual repository write, maintain, or admin permission checks for approval and human decisions, not issue author associations. Visible issue text remains untrusted; keep task approval explicit and use the configured human gate for decisions requiring a maintainer.

### Configuration migration

Remove obsolete `security.trusted_associations` and `security.trusted_reviewers` keys; strict configuration rejects unknown fields. Approval and human review now use the tracker actor's actual repository permission. Remove `--allow-harness-changes` automation: a protected-path exception now requires `allow_harness_changes: true` in the exact approved WorkUnit, not a CLI bypass. Existing tasks must receive a fresh immutable approval before execution; do not migrate an old comment marker or readiness label into authority.

## Draft, inspect, approve

```bash
swarm issue create \
  --title "Add retry policy" \
  --objective "Retry transient HTTP failures" \
  --constraint "Preserve the public client API" \
  --acceptance "Retries 429 responses within the configured limit" \
  --workflow feature --gate human

# Inspect/lint the actual issue, then approve its immutable revision.
swarm issue lint 42
swarm issue approve 42
```

`issue create` creates a **draft** by default. `--ready` is an explicit convenience to approve the newly created revision. Approval records the canonical WorkUnit digest, policy digest, and immutable base SHA in authoritative state before adding readiness. Edited WorkUnits, policy changes, withdrawn readiness, and changed harness permissions require the relevant reapproval; do not use comments as an approval API.

A WorkUnit can be issue-form fields, fenced YAML, or an approved external file reference. It must identify objective, constraints, acceptance criteria, workflow, and optional gate. The issue number selected by the tracker is authoritative; an embedded different number is rejected.

## Validation policy and protected paths

```yaml
required_checks:
  - name: test
    command: go test ./...
    timeout: 10m
protected_paths:
  - .github/workflows/**
  - .swarm/config.yaml
  - .swarm/validation/**
  - .swarm/contracts/**
  - .swarm/prompts/**
  - .swarm/workflows/**
```

The candidate copy is not authoritative. Validation resolves this policy from the approved base and compares the base, worktree, index, and untracked files. Protected policy, configuration, prompts, and workflows are governed by the approved baseline; candidate flags cannot broadly bypass them. At least one required check and protected-path pattern are mandatory. Generic repositories deliberately fail validation until project-specific checks are configured.

The approved WorkUnit's `allow_harness_changes: true` exception applies to task-scoped validation/submission, not `validate --ci`. Consumer CI deliberately continues to reject protected-path changes. Bootstrap the initial trusted baseline before enabling the required validation check; later policy promotion requires a separate maintainer-reviewed change and any narrowly authorized branch-rule exception. Do not weaken candidate checks to let a task rewrite its own authority.

## Plan gates

- `none`: implementation may follow a recorded plan without a reviewer verdict.
- `agent`: an isolated configured reviewer must emit exactly `VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED`; approval binds task, plan, and test-plan hashes.
- `human`: planning pauses. A permitted human records exactly one CLI decision using `swarm review <issue> --approve` or `--reject`; an agent cannot satisfy this gate.

The effective gate follows task/workflow/repository policy and a CLI request cannot lower a mandatory gate. See [workflows](WORKFLOWS.md) for commands and transitions.

## Dependencies and delivery baseline

Dependencies are normally ready only after their prerequisite is merged/closed. A Draft PR alone does not put prerequisite code in another worktree. Stacked dependencies require an explicitly supported checkpoint/base relationship; otherwise keep tasks independent or merge the prerequisite first.

The approved base is the remote default branch when `origin` exists (after fetch), otherwise local `HEAD` for an explicit local fixture. Worktrees and checkpoints are verified against the recorded branch/head; SwarmOS does not reset a surviving branch to recreate work.

Next: [developer operations and recovery](developer.md) · [architecture](ARCHITECTURE.md) · [security boundaries](../SECURITY.md)
