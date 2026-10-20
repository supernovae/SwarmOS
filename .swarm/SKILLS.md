# SwarmOS Agent & Harness Skills Manual

This repository is orchestrated with **SwarmOS**. Both headless agents (`swarm work`) and interactive coding harnesses (`claude`, `codex`, `opencode`, `omp`, or human-in-the-loop sessions) follow the same contract-driven workflow.

## 1. Interactive & Harness-Driven Task Lifecycle (`swarm stage`)

When working interactively or inside a coding harness without spawning nested agent subprocesses:

1. **Claim an approved issue and provision its isolated worktree**:
   ```bash
   swarm claim <issue-number>
   cd .swarm/worktrees/issue-<issue-number>
   ```
2. **Read the active contract and compiled system prompt**:
   - `.swarm/active/WORKUNIT.md` — immutable task objective, constraints, and acceptance criteria.
   - `.swarm/active/SYSTEM_PROMPT.md` — project goals, architecture, conventions, rules, and validation checks.
3. **Draft and stage the plan (`plan` phase)**:
   - Write `.swarm/active/PLAN.md` (implementation design, exact files, edge cases).
   - Write `.swarm/active/TEST_PLAN.md` (mapping every acceptance criterion to verification commands).
   - Stage the plan and record harness provenance on the issue:
     ```bash
     swarm stage <issue-number> --phase plan --harness <harness-name> --model <model-name>
     ```
4. **Run or record plan review (`review` phase)**:
   - For agent-gated tasks: `swarm review <issue-number>`
   - For human-gated tasks: maintainer runs `swarm review <issue-number> --approve --notes "LGTM"`
5. **Implement, validate, and stage the checkpoint (`implement` phase)**:
   - Implement the approved plan and run the checks in `.swarm/validation/required-checks.yaml`.
   - Stage the implementation checkpoint and validation attestation:
     ```bash
     swarm stage <issue-number> --phase implement --harness <harness-name> --model <model-name>
     ```
6. **Submit the Draft PR**:
   ```bash
   swarm submit <issue-number>
   ```

## 2. Project-Owner Backlog Planning & Sense-Making (`swarm plan`)

Maintainers and project owners can decompose roadmaps or audit open issues against `.swarm/goals.md` and `.swarm/rules/`:

- **Decompose a project plan or roadmap into structured WorkUnit issues**:
  ```bash
  swarm plan --from ROADMAP.md --apply
  ```
- **Scan, sense-make, and import open issues against project goals and touchpoints**:
  ```bash
  swarm plan --scan --apply --normalize
  ```
