<div align="center">

# 🐝 SwarmOS

**Your issues. Your agents. One coordinated workflow.**

[![CI & Validation](https://github.com/supernovae/SwarmOS/actions/workflows/swarm-validate.yml/badge.svg)](https://github.com/supernovae/SwarmOS/actions/workflows/swarm-validate.yml)
[![Security Scanning](https://github.com/supernovae/SwarmOS/actions/workflows/security.yml/badge.svg)](https://github.com/supernovae/SwarmOS/actions/workflows/security.yml)
[![Go Version](https://img.shields.io/badge/Go-1.26.10+-00ADD8?style=flat-square&logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg?style=flat-square)](LICENSE)
[![Agents](https://img.shields.io/badge/Agents-Claude%20%7C%20Codex%20%7C%20OpenCode%20%7C%20OhMyPi-7B61FF?style=flat-square)](#bring-your-own-agents)

*Turn GitHub Issues into structured work contracts, coordinate multiple coding agents without a separate server, isolate every task in its own Git worktree, and ship artifact-backed Draft PRs.*

[**Quickstart**](#quickstart) · [**How It Works**](#how-it-works) · [**Grok Bot Guide**](docs/grokbot.md) · [**OpenAI Dot Guide**](docs/openai-dot.md) · [**Project Setup**](docs/project.md) · [**Developer Guide**](docs/developer.md)

</div>

---

## Why SwarmOS?

Single-agent coding tools are great at editing files, but coordinating several agents across a real backlog is where things usually fall apart: agents step on the same task, skip design review, dirty your main checkout, or open pull requests without proof that your checks ran.

**SwarmOS (`swarm`)** gives your agents and teammates a shared operating model directly inside your repository: **GitHub Issues define the work, Git compare-and-swap refs coordinate ownership and checkpoints, and isolated worktrees keep every task clean.** Whether you are an **open-source maintainer** letting contributors around the world point their coding agents at approved issues, or an **engineering organization** orchestrating internal agents across private repositories without running a separate control plane, your repo already has everything it needs.

| Challenge | How SwarmOS Solves It |
| :--- | :--- |
| **Vague issue prompts** lead to scope drift | **Schema-Validated WorkUnits** capture an explicit `Objective`, `Constraints`, `Acceptance Criteria`, `Workflow`, and `Plan Gate`, approved by a maintainer before work begins. |
| **Agents colliding on the same issue** | **Git Compare-and-Swap Claims** give each attempt a time-bounded lease and fencing token so concurrent workers never overwrite each other. |
| **Unreviewed code dumps** | **Plan-First Execution** writes `PLAN.md` and `TEST_PLAN.md` first, then runs your chosen `none`, `agent`, or `human` review gate before touching implementation code. |
| **Dirty working trees & lost progress** | **Isolated Git Worktrees** provision every task in `.swarm/worktrees/issue-<N>` on branch `swarm/issue-<N>-<slug>` with transferable Git checkpoints. |
| **Vendor lock-in to a single CLI** | **Multi-Agent Routing** lets you mix **Claude Code (`claude`), Codex (`codex`), OpenCode (`opencode`), and OhMyPi (`omp`)**—and coordinate from [**Grok Bot**](docs/grokbot.md) or [**OpenAI Dot**](docs/openai-dot.md). |
| **Untested or unsafe diffs** | **Baseline Validation** runs the required checks and protected-path rules from the approved base commit before opening an artifact-backed Draft PR. |

---

## How It Works

```mermaid
flowchart LR
    subgraph GitHub["GitHub Repository & Backlog"]
        Issue["Approved Issue #N<br/>WorkUnit Contract"]
        State["Git CAS State Ref<br/>Claim + Checkpoints + Audit Trail"]
        PR["Draft Pull Request<br/>Checklist + Artifacts"]
    end

    subgraph Worktree["Isolated Worktree (.swarm/worktrees/issue-N)"]
        Plan["1. Planner<br/>PLAN.md + TEST_PLAN.md"]
        Gate{"2. Review Gate<br/>none | agent | human"}
        Impl["3. Developer<br/>Implementation"]
        Val["4. Validator<br/>Baseline Checks"]
    end

    Issue -->|swarm claim / work| State
    State -->|git worktree| Plan
    Plan --> Gate
    Gate -->|Approved| Impl --> Val
    Val -->|swarm submit| PR
```

1. **Create & Approve (`swarm issue create` / `swarm issue approve`)**: Author a structured WorkUnit issue via the CLI or GitHub Issue Form, then approve its revision when it is ready for agents to pick up.
2. **Claim & Provision (`swarm claim` / `swarm work`)**: SwarmOS acquires a fenced lease, creates `.swarm/worktrees/issue-<N>`, and compiles `.swarm/active/WORKUNIT.md` and `SYSTEM_PROMPT.md`.
3. **Plan & Review (`PLAN.md` + `TEST_PLAN.md`)**: The planner maps out the file-level design and acceptance tests. Depending on your gate (`agent`, `human`, or `none`), an isolated reviewer agent audits the plan or the workflow pauses for a maintainer's `swarm review <issue> --approve`.
4. **Implement, Validate & Submit (`swarm validate` / `swarm submit`)**: The developer implements the approved plan, SwarmOS verifies protected paths and required checks against the approved baseline, and `swarm submit` opens a Draft PR with the full artifact trail.

---

## Quickstart

### 1. Install `swarm`

Download the latest prebuilt binary for **Linux**, **macOS**, or **Windows** from [**GitHub Releases**](https://github.com/supernovae/SwarmOS/releases/latest) (or browse [all releases](https://github.com/supernovae/SwarmOS/releases)), or install `v0.3.0` with Go 1.26.10+:

```bash
go install github.com/supernovae/swarmos/cmd/swarm@v0.3.0
```

### 2. Initialize Your Repository

From a Git checkout connected to GitHub (`gh auth login`), scaffold your `.swarm/` harness and run diagnostics:

```bash
swarm init --sync-labels --backend github --swarm-version v0.3.0
swarm doctor --backend github
```

Example `swarm doctor` output:

```text
CATEGORY   CHECK                                    STATUS   DETAIL
--------   -----                                    ------   ------
system     git                                      OK       git version 2.54.0
tracker    gh-cli                                   OK       /opt/homebrew/bin/gh
harness    .swarm/config.yaml                       OK       v1 (default_agent=claude, default_gate=agent)
harness    .swarm/validation/required-checks.yaml   OK       3 checks, 6 protected paths
agent      claude                                   OK       /opt/homebrew/bin/claude
agent      codex                                    OK       /opt/homebrew/bin/codex
agent      omp                                      OK       /opt/homebrew/bin/omp
agent      opencode                                 OK       /opt/homebrew/bin/opencode
```

Commit the generated `.swarm/`, `.github/`, `.claude/`, and `.opencode/` files to your default branch so every teammate and agent works from the same project rules.

### 3. Create, Approve & Ship a Task

```bash
# 1. Create a structured WorkUnit issue (starts as a draft; pass --ready to approve immediately)
swarm issue create --backend github \
  --title "Add JSON output to the status command" \
  --objective "Let scripts read application status as JSON" \
  --constraint "Keep the existing text output unchanged" \
  --acceptance "status --json emits valid JSON with the current status" \
  --workflow feature --gate agent

# 2. Approve the issue when the scope looks right (replace 42 with your issue number)
swarm issue approve 42 --backend github

# 3. Preview the resolved agents and gate, then run the workflow and open a Draft PR
swarm work 42 --dry-run --backend github
swarm work 42 --backend github
swarm submit 42 --backend github
```

> [!TIP]
> `swarm work` runs headlessly by default. Pass `--headless=false` to pair with your agent, or use `swarm node run --watch` to keep picking up approved tasks. The default isolated path uses Codex's native sandbox and your existing `codex login` on supported Linux setups. Start with `swarm doctor`; the [worker guide](docs/developer.md#choose-how-you-work) covers setup and other harnesses. Use `--backend local` to experiment with local coordination storage.

---

## Bring Your Own Agents

SwarmOS supports **Claude Code (`claude`), OpenAI Codex (`codex`), OpenCode (`opencode`), and OhMyPi (`omp`)**. Use a single tool across the workflow or route each role to your favorite harness:

```bash
swarm work 42 \
  --planner-agent claude \
  --reviewer-agent omp \
  --developer-agent codex \
  --tester-agent opencode
```

Want to coordinate work from an always-on conversational teammate?
- **[Working with Grok Bot & SwarmOS](docs/grokbot.md)** — Connect Grok Bot to GitHub, chat with your repository, request PR reviews, or let Grok drive `swarm` on its cloud computer.
- **[Working with OpenAI Dot, Codex & SwarmOS](docs/openai-dot.md)** — Add the repositories you want to swarm on to your Codex Cloud environment (or connected dev machine) and let your Dot coordinate tasks end to end.

---

## Team Handoffs & Multi-Repo Worker Pools

Register the repositories you work on so teammates and worker pools can pull from a shared backlog:

```bash
# Register this checkout in ~/.config/swarm/repos.yaml
swarm repo add --backend github

# Checkpoint a completed plan so another teammate or worker can pick up the next phase
swarm work 42 --phase plan --handoff --backend github

# Claim and run the next ready task across all registered repositories
swarm work --next --all --backend github

# Run a bounded worker pool that drains up to 4 tasks and opens Draft PRs
swarm node init
swarm node run --all --concurrency 2 --max-jobs 4 --auto-submit --backend github
```


---

## Fork-Based Open-Source Contributions (`--fork`)

External contributors do not need push access to the upstream repository. When maintainers opt in to the upstream fork coordinator Action, contributors can work in isolated local worktrees, push checkpoints to their own personal GitHub fork, and open cross-repository Draft PRs—while issue approval, validation policy, human plan review, and PR merge remain upstream maintainer actions.

```bash
# Maintainer (once, on the upstream default branch):
# Set SWARM_VERSION_PIN to an immutable published fork-capable release tag or full 40-char commit SHA
export SWARM_VERSION_PIN="<published-fork-capable-tag-or-full-commit-sha>"
swarm init --fork-coordinator --swarm-version "$SWARM_VERSION_PIN"
swarm issue approve 42 --backend github

# Contributor (clones upstream as origin, adds writable personal fork as "fork"):
git clone https://github.com/OWNER/REPO.git
cd REPO
gh repo fork --remote=true --remote-name=fork

# Claim, work on local branch swarm/issue-42-<claim_id>, push checkpoints to "fork",
# and open a fork-qualified Draft PR (<fork-owner>:swarm/issue-42-<claim_id> -> upstream):
swarm --fork claim 42
swarm --fork work 42
swarm --fork renew 42   # optional when you need additional lease time
swarm --fork submit 42
```

See the [**Fork Mode Guide (`docs/fork-mode.md`)**](docs/fork-mode.md) for architecture details, per-contributor claim caps (`fork.max_active_claims`), and security boundaries.
---

## CLI Overview

| Command | What It Does |
| :--- | :--- |
| `swarm init [--force] [--sync-labels] [--template-dir <dir>] [--swarm-version <ref>] [--fork-coordinator]` | Scaffold `.swarm/`, `AGENTS.md`, `.swarm/SKILLS.md`, GitHub Issue Forms, consumer CI / fork-coordinator workflows, and `/swarm-*` slash commands. |
| `swarm doctor` | Check `git`, `gh`, GitHub `Epic` issue type & maintainer auth, `.swarm/` schemas, and configured agent binaries. |
| `swarm issue create\|epic create\|approve\|lint\|decompose` | Create draft WorkUnits (`--parent`), standalone Epics (`--body-file`), approve revisions, lint schemas, or split WorkUnits into linked draft sub-issues. |
| `swarm plan [--from <file> \| --scan] [--normalize] [--apply] [--ready]` | Preview `Step N` tasks from a roadmap, publish a native `Epic` with linked draft child WorkUnits (`--apply`), or scan and triage open issues (`--scan`). |
| `swarm claim <issue>` / `swarm pull [--all]` | Claim a specific approved issue or pull the next ready task across registered repositories. |
| `swarm work [<issue>] [--next] [--all]` | Run `plan`, `review`, `implement`, and `validate` (supports `--dry-run`, `--handoff`, and role overrides). |
| `swarm stage [<issue>] [--phase plan\|implement] [--harness <name>] [--model <name>]` | Stage interactive or harness-produced plan and implementation artifacts onto an active lease with provenance metadata. |
| `swarm review <issue> [--approve\|--reject]` | Run an isolated agent review or record a maintainer's human gate decision. |
| `swarm validate [<issue>] [--ci --base <sha>]` | Verify artifacts, check protected paths, and run `.swarm/validation/required-checks.yaml`. |
| `swarm submit [<issue>]` | Clean temporary markers, checkpoint and push the worktree branch, and open or update a Draft PR with collapsible artifacts and provenance attestation. |
| `swarm status [<issue>] [--all]` | Inspect active claims, lease TTL, current phase, and published artifacts. |
| `swarm repo add\|list\|remove` | Manage the local multi-repository registry (`~/.config/swarm/repos.yaml`). |
| `swarm node init\|status\|run` | Configure a worker node profile and run concurrent worker pools (`--concurrency`, `--max-jobs`, `--watch`). |
| `swarm renew <issue>` / `swarm release <issue>` | Extend an active lease's TTL or release a claim back to the queue. |
| `swarm fork-coordinator --event <path> [--json]` | Process a verified `issue_comment.created` fork worker request inside the upstream GitHub Action. |
| `swarm version [--json]` | Print version, commit, and platform metadata. |

Every lifecycle command supports `--json` for scripts and automation, and worker commands accept `--fork [--fork-remote <name>]` for personal-fork contributions.

---

## Documentation & Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](docs/ADMIN_GUIDE.md)** — Daily maintainer playbook for roadmap decomposition (`swarm plan --from`), backlog scanning (`swarm plan --scan`), and auditing provenance-attested Draft PRs.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](docs/GITHUB.md)** — Configure repository rulesets, `SWARM_GITHUB_TOKEN` auth without `gh` CLI, and role-separated collaborator allowlists.
- **[Fork Mode Guide (`docs/fork-mode.md`)](docs/fork-mode.md)** — Contribute from a personal GitHub fork without upstream push access using the upstream fork coordinator Action.
- **[Working with Grok Bot (`docs/grokbot.md`)](docs/grokbot.md)** — Connect Grok Bot to your repositories, talk to your codebase, run PR reviews, and coordinate SwarmOS workers.
- **[Working with OpenAI Dot & Codex (`docs/openai-dot.md`)](docs/openai-dot.md)** — Configure Codex Cloud environments and remote repositories so your Dot can plan, review, and ship changes.
- **[Project & Maintainer Setup (`docs/project.md`)](docs/project.md)** — Configure `.swarm/config.yaml`, validation rules, plan gates, and repository protections.
- **[Developer & Worker Guide (`docs/developer.md`)](docs/developer.md)** — Everyday CLI workflows, interactive `swarm stage`, agent harness table, reviewer sandboxing, handoffs, and recovery.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](docs/WORKFLOWS.md)** — Role prompts, `PLAN.md` / `TEST_PLAN.md` / `REVIEW.md` artifacts, provenance attestations, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](docs/ARCHITECTURE.md)** — Git compare-and-swap state refs, claim fencing, worktree checkpoints, and process boundaries.
- **[Security Policy (`SECURITY.md`)](SECURITY.md)** — Trust model, reviewer isolation, vulnerability reporting, and release attestation verification.
- **[Agent & Contributor Guide (`AGENTS.md`)](AGENTS.md)** — Repository conventions, documentation voice guardrails, and review rules for coding agents and contributors.
