# AGENTS.md

This repository uses **SwarmOS** for contract-driven software delivery.

## Quick Reference for Coding Agents

- **Agent Factory & Staging Manual**: Read `.swarm/SKILLS.md` for step-by-step instructions on claiming issues (`swarm issue claim`), staging plans and implementations (`swarm issue stage`), reviewing implementation plans (`swarm issue review-plan`), and submitting Draft PRs (`swarm issue submit`).
- **Project Goals & Safety Invariants**: Read `.swarm/goals.md` and `.swarm/rules/default.md`.
- **Architecture & Conventions**: Read `.swarm/project.md`, `.swarm/architecture.md`, and `.swarm/conventions.md`.
- **Validation Policy**: Read `.swarm/validation/required-checks.yaml` as a coordinator-owned reference, not a per-role execution list. Roles follow focused checks and independent review/reuse in TEST_PLAN.md; the coordinator runs the complete trusted-base required policy once after roles finish and before publication. Agent results remain advisory.
- **Active Worktree Context**: When working inside a provisioned task worktree (`.swarm/worktrees/issue-<N>`), always read `.swarm/active/WORKUNIT.md` and `.swarm/active/SYSTEM_PROMPT.md` first.
