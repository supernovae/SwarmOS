# Workflows, Role Contracts & Review Gates

SwarmOS uses a **Plan-First** lifecycle across all supported coding harnesses (`claude`, `codex`, `opencode`, `omp`). Every task produces a concrete design and test plan before implementation code is written.

---

## 1. Built-In Workflow Recipes (`.swarm/workflows/`)

SwarmOS scaffolds three workflow recipes in `.swarm/workflows/`:
- **`feature.yaml`**: End-to-end feature delivery from requirements analysis and design through implementation and validation.
- **`bugfix.yaml`**: Root-cause analysis, regression test planning, fix implementation, and verification.
- **`refactor.yaml`**: Structural improvements that preserve behavior and pass all repository checks.

Workflow YAML files customize the step descriptions and role context compiled into `.swarm/active/WORKUNIT.md` and `SYSTEM_PROMPT.md`, while the SwarmOS engine enforces the execution sequence:

```mermaid
flowchart LR
    Plan["1. Planner<br/>PLAN.md + TEST_PLAN.md"] --> Gate{"2. Plan Review Gate<br/>none | agent | human"}
    Gate -->|Approved| Impl["3. Developer<br/>Code + Tests"]
    Impl --> Val["4. Tester & Validator<br/>Baseline Checks"]
    Val --> Submit["5. Explicit Submit<br/>Draft PR"]
    Submit --> Review["6. GitHub Code & Acceptance Review"]
    Review --> Merge["7. Merge"]
```

---

## 2. Role Contracts & Artifacts

Each workflow phase binds to a role prompt in `.swarm/prompts/` and reads/writes well-defined artifacts in `.swarm/active/`:

| Role | Prompt File | Reads | Produces |
| :--- | :--- | :--- | :--- |
| **Planner** (`planner`) | `.swarm/prompts/planner.md` | `WORKUNIT.md` and repository context | `.swarm/active/PLAN.md` (`design`) & `.swarm/active/TEST_PLAN.md` (`test-plan`) |
| **Reviewer** (`reviewer`) | `.swarm/prompts/reviewer.md` | `WORKUNIT.md`, `PLAN.md`, `TEST_PLAN.md` | `.swarm/active/REVIEW.md` (`review` starting with `VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED`) |
| **Developer** (`developer`) | `.swarm/prompts/developer.md` | Approved `PLAN.md` and `TEST_PLAN.md` | Working-tree implementation changes + checkpoint commit |
| **Tester / Validator** (`tester`) | `.swarm/prompts/tester.md` | Implementation and approved-base validation policy | Validation check output (`validation` artifact) |

SwarmOS compiles `.swarm/active/WORKUNIT.md` and `.swarm/active/SYSTEM_PROMPT.md` freshly for each claimed task and injects managed pointer blocks into `AGENTS.md` and `CLAUDE.md` inside the worktree so interactive and headless tools find the active contract immediately.

---

## 3. Plan Review Gates (`none | agent | human`)

```bash
# Preview the effective workflow, resolved gate, and role agents without side effects
swarm issue work 42 --dry-run --backend github

# Agent gate: planner writes PLAN.md + TEST_PLAN.md, then the sandboxed reviewer audits it
swarm issue work 42 --gate agent --planner-agent claude --reviewer-agent omp --developer-agent codex --backend github

# Human gate: generate and publish the plan, pause for maintainer review, then resume
swarm issue work 42 --gate human --phase plan --backend github
swarm issue review-plan 42 --approve --notes "Plan looks great; proceed with implementation" --backend github
swarm issue work 42 --phase implement --backend github
```

How gate verification keeps reviews trustworthy:
- **Cryptographic plan binding**: Both `agent` and `human` approvals bind to the exact `WorkUnit` digest and the SHA-256 digests of `PLAN.md` and `TEST_PLAN.md`. If the issue or plan files are edited after approval, SwarmOS requires a fresh review before implementation or submission.
- **Exact reviewer verdicts**: Under `--gate agent`, the isolated reviewer must begin `.swarm/active/REVIEW.md` with `VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED`. Any `CHANGES_REQUESTED` verdict stops the workflow before code is modified and publishes the findings for inspection.
- **Authenticated human decisions**: Under `--gate human`, planning pauses with label `swarm:plan-review` until a repository maintainer runs `swarm issue review-plan <issue> --approve` or `swarm issue review-plan <issue> --reject`.

`swarm issue approve <issue>` makes the issue's WorkUnit the approved work contract before execution; plan approval is a second, separate checkpoint. `swarm issue validate` runs the approved-base checks and protected-path policy, not a pass/fail decision for each acceptance criterion. `swarm issue submit <issue>` links a Draft PR proposing implementation of the issue contract; `swarm issue status <issue>` exposes its PR number and URL. Maintainers use GitHub for final code and acceptance review. PR repair and rebase are not available as SwarmOS commands. See [Taking a Submitted PR Through Acceptance Review](developer.md#taking-a-submitted-pr-through-acceptance-review) for the handoff.

---

## 4. Interactive Slash Commands & Shared Skills

SwarmOS separates per-issue execution planning from repository-wide roadmap planning (and never overrides a harness's built-in `/plan` command):

- **Inside a claimed task worktree (`.swarm/worktrees/issue-<N>`)**:
  - `/swarm-plan` — Draft `.swarm/active/PLAN.md` and `.swarm/active/TEST_PLAN.md` for the active WorkUnit.
  - `/swarm-review` — Audit the current plan against all constraints and acceptance criteria.
  - `/swarm-implement` — Implement the approved plan step by step.
  - `/swarm-validate` — Run the approved checks and test-plan scenarios, reporting observed results and unverified criteria. The coordinator runs claim-bound `swarm issue validate` and publishes its validation report.
- **At the repository root for roadmap ideation & backlog triage**:
  - `/swarm-project-plan` (in Claude Code and OpenCode) or the shared `.swarm/skills/triage-and-plan.md` skill (used as a regular prompt in Codex and `omp`) — Collaboratively refine an idea into a repo-local plan file, preview `Step N` child tasks with `swarm repo plan --from <file>`, publish a native `Epic` and draft child sub-issues on explicit maintainer instruction (`--apply`), and selectively approve child WorkUnits with `swarm issue approve <N>`, or triage open issues with `swarm repo plan --scan`.

## 5. Headless Progress

`swarm issue work` reports stage starts, completions, pauses, and handoffs to stderr. During a quiet stage, a local heartbeat prints every 15 seconds with the current stage and elapsed time. This timer reports that execution is still running; lease renewal continues separately.

The final result stays on stdout, including when using `--json`. Pass `--progress=false` to silence progress messages. Interactive execution and dry runs do not print these messages.

Progress messages contain stage metadata only. Failed harness runs report the role, exit status when available, and cancellation or timeout without printing captured transcripts, command arguments, or nested harness diagnostics.

## Related Guides

- **[Project & Maintainer Setup (`docs/project.md`)](project.md)** — Initialize `.swarm/`, configure agents, and approve issues.
- **[Developer & Worker Guide (`docs/developer.md`)](developer.md)** — Everyday CLI commands, handoffs, recovery, and worker pools.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS coordination, claim fencing, and checkpoint design.
