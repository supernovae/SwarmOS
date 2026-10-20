# SwarmOS Workflows & Plan Review Gates

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
| `developer` | `.swarm/prompts/developer.md` | Implement `PLAN.md` step-by-step and write tests from `TEST_PLAN.md`. | Working tree code changes |
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
swarm work 12 --gate agent --agent claude --reviewer-agent omp --headless
```
After the planner produces `PLAN.md` and `TEST_PLAN.md`, SwarmOS invokes the reviewer agent (`--reviewer-agent`, which can be a different model/harness such as `omp` or `codex`).
- If `.swarm/active/REVIEW.md` begins with `VERDICT: APPROVED`, the `swarm:plan-approved` label is applied and implementation starts automatically.
- If it begins with `VERDICT: CHANGES_REQUESTED`, the `swarm:plan-review` label is applied, the findings are posted to the issue, and execution halts before touching code.

### `human` (Human-in-the-Loop Approval Gate)
```bash
# Phase 1: Generate and publish the plan, then pause for human review
swarm work 12 --gate human --agent claude --headless

# Option A: Approve from the CLI
swarm review 12 --approve --notes "LGTM, proceed with implementation"

# Option B: Comment `/swarm approve-plan` directly on the GitHub Issue

# Phase 2: Resume implementation after approval
swarm work 12 --phase implement --agent claude --headless
```

---

## 3. Authoring WorkUnit Issues

### Format A: GitHub Issue Form (Recommended for Teams)
Open a new issue on GitHub using the **SwarmOS Work Unit** template (`.github/ISSUE_TEMPLATE/swarm-workunit.yml`), which renders structured inputs for Objective, Constraints, Acceptance Criteria, Workflow, and Plan Gate.

### Format B: Fenced YAML in Any Issue Body
```yaml swarm-workunit
id: ISSUE-42
objective: Add OAuth2 device flow support to the CLI auth command
constraints:
  - Do not break existing token file format
  - Use standard library net/http only
acceptance:
  - Device flow polling handles slow_down and authorization_pending
  - Unit tests cover token persistence and timeout
workflow: feature
gate: agent
```

### Format C: External Repo Contract File
Reference a versioned YAML file in the issue body:
```text
swarm-workunit-path: .swarm/workunits/oauth-device-flow.yaml
```

---

## 4. Pulling Work Down & Multi-Repository Dispatch

### Single-Project Work Pulling
Inside any SwarmOS-initialized repository, contributors can pull the next unclaimed `swarm:ready` issue without looking up issue numbers:

```bash
# Claim and provision the next unclaimed swarm:ready issue into .swarm/worktrees/issue-<N>
swarm pull --agent claude

# Or claim and execute the full Plan -> Gate -> Implement -> Validate workflow in one step
swarm work --next --agent claude --headless
```

### Multi-Repository Registry (`~/.config/swarm/repos.yaml`)
To work across multiple repositories from any directory on your machine:

```bash
# 1. Register local checkouts (or pass --clone to clone from GitHub into ~/.swarm/repos/<owner>/<repo>)
swarm repo add supernovae/SwarmOS ~/src/SwarmOS
swarm repo add supernovae/AnotherService --clone

# 2. Inspect registered repositories
swarm repo list

# 3. View unified status across all registered repositories
swarm status --all

# 4. Target a registered repository from anywhere without cd or --root
swarm work 5 --repo supernovae/SwarmOS --agent omp

# 5. Pull and execute the next ready work unit across ALL registered repositories
swarm work --next --all --agent claude --reviewer-agent omp --gate agent --headless
```

---

## 5. Multi-Role Agent Routing, Team Handoffs & Volunteer Nodes (Phases 2–4)

### Per-Role Agent Routing (Phase 2)
You can assign a dedicated agent harness to each role via CLI flags or `.swarm/config.yaml`:

```yaml
# .swarm/config.yaml
roles:
  planner: claude
  reviewer: omp
  developer: codex
  tester: opencode
```

Or per-invocation:
```bash
swarm work 12 \
  --planner-agent claude \
  --reviewer-agent omp \
  --developer-agent codex \
  --tester-agent opencode \
  --gate agent --headless
```

### Work Partitioning, Dependency Gating & Collaborative Handoffs (Phase 3)
- **Work Partitioning (`swarm decompose <issue>`)**: Splits an epic or multi-criterion issue into child `WorkUnit` issues linked via `parent_issue` and optional sequential `depends_on` constraints. `swarm pull` and `swarm work` automatically block work units whose `depends_on` prerequisites are not yet completed.
- **Collaborative Phase Handoffs (`--handoff`)**: A planner or architect can run `swarm work 12 --phase plan --handoff`, publishing the `design` and `test-plan` artifacts and placing the lease in `status: handoff` (`next_phase: review` or `implement`). Any teammate or volunteer node running `swarm pull` or `swarm work --next` can then claim the next phase while preserving the full artifact and contributor history.

### Distributed Volunteer Nodes (Phase 4)
Register a machine's capabilities (`coding`, `testing`, `review`, `docs`), installed models, and hardware resources (`OS`, `Arch`, `CPUs`, `RAM`) in `~/.config/swarm/node.yaml`:

```bash
swarm node init --capabilities coding,testing,review,docs
swarm node status

# Run a 4-worker parallel pool across all registered repositories and auto-submit passing Draft PRs
swarm node run --all --concurrency 4 --once --gate agent --auto-submit
```
