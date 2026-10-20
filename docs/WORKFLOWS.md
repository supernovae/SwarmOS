# SwarmOS Workflows & Role Contracts

SwarmOS enforces a **Plan-First** engineering lifecycle across all supported coding agents (`claude`, `codex`, `opencode`, `omp`).

---

## 1. Built-In Workflows (`.swarm/workflows/`)

SwarmOS ships with three default workflows in `.swarm/workflows/`:
- **`feature.yaml`**: End-to-end feature delivery (`analyze` $\rightarrow$ `design` $\rightarrow$ `test_plan` $\rightarrow$ `review_plan` $\rightarrow$ `implement` $\rightarrow$ `validate` $\rightarrow$ `create_pr`).
- **`bugfix.yaml`**: Root-cause analysis, regression test planning, fix implementation, and verification.
- **`refactor.yaml`**: Structural refactoring preserving behavioral invariants and passing all checks.

Each workflow step binds to one of four role prompts in `.swarm/prompts/`:

| Role | Prompt File | Responsibility | Output Artifact |
| :--- | :--- | :--- | :--- |
| `planner` | `.swarm/prompts/planner.md` | Inspect repository without modifying production files; design file-level implementation steps and map acceptance criteria to test commands. | `.swarm/active/PLAN.md` (`design`) & `.swarm/active/TEST_PLAN.md` (`test-plan`) |
| `reviewer` | `.swarm/prompts/reviewer.md` | Audit `PLAN.md` and `TEST_PLAN.md` against `Objective`, `Constraints`, `Acceptance`, and protected paths. | `.swarm/active/REVIEW.md` (`review` with `VERDICT: APPROVED` or `CHANGES_REQUESTED`) |
| `developer` | `.swarm/prompts/developer.md` | Implement `PLAN.md` step-by-step and write tests from `TEST_PLAN.md`. | Working tree code changes + `implementation` artifact |
| `tester` | `.swarm/prompts/tester.md` | Run `swarm validate` and verify every acceptance criterion passes. | `.swarm/active/VALIDATION.md` (`validation`) |

---

## 2. Plan Approval Gates (`none | agent | human`)

You can configure the plan gate per issue, per workflow (`default_gate` in `.swarm/workflows/*.yaml`), in `.swarm/config.yaml`, or on the CLI via `--gate`:

### `none` (Fast Autonomous Loop)
```bash
swarm work 12 --gate none --agent claude --headless
```
Runs the `planner` role to produce `PLAN.md` and `TEST_PLAN.md`, publishes both artifacts to the issue comment, and immediately transitions to the `developer` role and `swarm validate`.

### `agent` (Automated Cross-Agent Review Gate)
```bash
swarm work 12 --gate agent --planner-agent claude --reviewer-agent omp --developer-agent codex --headless
```
After the planner produces `PLAN.md` and `TEST_PLAN.md`, SwarmOS invokes the reviewer agent (`--reviewer-agent`, which can be a different model/harness such as `omp` or `codex`).
- If `.swarm/active/REVIEW.md` begins with `VERDICT: APPROVED`, the `swarm:plan-approved` label is applied and implementation starts automatically.
- If it begins with `VERDICT: CHANGES_REQUESTED`, the `swarm:plan-review` label is applied, the findings are posted to the issue, and execution halts before touching code.

### `human` (Human-in-the-Loop Approval Gate)
```bash
# Step 1: Generate and publish the plan, then pause for human review
swarm work 12 --gate human --agent claude --headless

# Option A: Approve from the CLI
swarm review 12 --approve --notes "LGTM, proceed with implementation"

# Option B: Comment `/swarm approve-plan` directly on the GitHub Issue

# Step 2: Resume implementation after approval
swarm work 12 --phase implement --agent claude --headless
```

---

## 3. Related Guides

- **[Project & Maintainer Guide (`docs/project.md`)](project.md)** — Authoring `WorkUnit` issues, configuring `.swarm/config.yaml`, and partitioning epics with `swarm decompose`.
- **[Developer, Team & Worker Node Guide (`docs/developer.md`)](developer.md)** — Pulling work (`swarm pull`), collaborative team handoffs (`--handoff`), multi-repo registries (`swarm repo`), and parallel worker pools (`swarm node run`).
