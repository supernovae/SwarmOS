# Skill: Backlog Triage, Issue Import & Execution Wave Planning

Use this skill when running `/swarm-project-plan` or `swarm plan` to help maintainers turn a project roadmap or raw issue backlog into safe, dependency-ordered SwarmOS WorkUnits.

## Triage Label Taxonomy
- `swarm:draft`: Valid structured `WorkUnit` awaiting maintainer review and `swarm issue approve <N>`.
- `swarm:needs-scope`: Raw or imported issue missing structured `Objective`, `Constraints`, or `Acceptance Criteria` sections.
- `swarm:blocked`: Issue waiting on open `depends_on` prerequisites, overlapping file touchpoints, or protected-path policy review.
- `swarm:ready`: Approved immutable `WorkUnit` ready for agents to claim (`swarm claim` or `swarm work --next`).

## Decomposition & Sense-Making Checklist
1. Verify each proposed or open issue aligns with `.swarm/goals.md` and `.swarm/rules/`.
2. Identify concrete file or package touchpoints for each issue.
3. Chain issues that touch the same files or packages via `depends_on` (or sequential execution waves) so concurrent agents never conflict.
4. Flag any issue that touches protected paths without `allow_harness_changes: true`.
