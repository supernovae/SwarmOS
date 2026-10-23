# SwarmOS Agent & Harness Skills Manual

This repository is orchestrated with **SwarmOS**. Both headless agents (`swarm issue work`) and interactive coding harnesses (`claude`, `codex`, `opencode`, `omp`, or human-in-the-loop sessions) follow the same contract-driven workflow.

## 1. Interactive & Harness-Driven Task Lifecycle (`swarm issue stage`)

When working interactively on a single approved `WorkUnit` issue (using `/swarm-plan`, `/swarm-review`, `/swarm-implement`, `/swarm-validate` in Claude Code or OpenCode, or regular prompts in Codex and `omp`):

1. **Claim an approved issue and provision its isolated worktree**:
   ```bash
   swarm issue claim '<issue-number>'
   cd '.swarm/worktrees/issue-<issue-number>'
   ```
2. **Read the active contract and compiled system prompt**:
   - `.swarm/active/WORKUNIT.md` — immutable task objective, constraints, and acceptance criteria.
   - `.swarm/active/SYSTEM_PROMPT.md` — project goals, architecture, conventions, rules, and validation checks.
3. **Draft and stage the plan (`plan` phase)**:
   - Write `.swarm/active/PLAN.md` (implementation design, exact files, edge cases).
   - Write `.swarm/active/TEST_PLAN.md` (mapping every acceptance criterion to verification commands).
   - Stage the plan and record harness provenance on the issue:
     ```bash
     swarm issue stage '<issue-number>' --phase plan --harness '<harness-name>' --model '<model-name>'
     ```
4. **Run or record plan review (`review` phase)**:
   - For agent-gated tasks: `swarm issue review-plan <issue-number>`
   - For human-gated tasks: maintainer runs `swarm issue review-plan <issue-number> --approve --notes "LGTM"`
5. **Implement, validate, and stage the checkpoint (`implement` phase)**:
   - Implement the approved plan and run the checks in `.swarm/validation/required-checks.yaml`.
   - Stage the implementation checkpoint and validation attestation:
     ```bash
     swarm issue stage '<issue-number>' --phase implement --harness '<harness-name>' --model '<model-name>'
     ```
6. **Submit the Draft PR**: The approved issue remains the work contract; submission links its proposed implementation to a draft PR for GitHub code and acceptance review.
   ```bash
   swarm issue submit '<issue-number>'
   ```

## 2. Repository-Wide Roadmap Ideation, Epics & Backlog Triage (`swarm repo plan`)

Maintainers use `swarm repo plan` (with `/swarm-project-plan` in Claude Code / OpenCode, or by referencing `.swarm/skills/triage-and-plan.md` in regular prompts with Codex / `omp`) to turn repository-wide ideas into a reviewed plan file, a non-executable parent `Epic` issue, and linked draft child `WorkUnit` issues:

1. **Interactive Idea $\rightarrow$ Repo-Local Plan File**:
   - Discuss the idea and outcome, inspect `.swarm/goals.md`, `.swarm/rules/`, implementation code, and open issues, and agree on independent `## <title>` tasks with `### Objective`, `### Constraints`, observable `### Acceptance Criteria` checkboxes, and explicit `depends on: <earlier-step-number>` lines.
   - Write the plan to a user-chosen repo-local file (without overwriting existing files unless instructed).
2. **Preview `Step N` Decomposition (Read-Only)**:
   ```bash
   swarm repo plan --from ROADMAP.md --backend github
   ```
3. **Publish Native Epic & Draft Child WorkUnits (Maintainer Only)**:
   - Only on explicit maintainer instruction, publish the unlabeled `Epic` issue (`type: Epic`) containing the full plan and attach draft child `WorkUnit` sub-issues (`swarm:draft`):
     ```bash
     swarm repo plan --from ROADMAP.md --apply --backend github
     ```
   - Selectively approve child `WorkUnit` issues when ready for workers to claim (never approve or execute the `Epic` issue itself):
     ```bash
     swarm issue approve <child-issue-number> --backend github
     ```
4. **Scan, Sense-Make & Triage Open Issues (`--scan`)**:
   - Audit open issues (including `swarm:community` intake reports) against `.swarm/goals.md`, `.swarm/rules/`, and file touchpoints:
     ```bash
     swarm repo plan --scan --backend github
     swarm repo plan --scan --apply --normalize --backend github
     ```
