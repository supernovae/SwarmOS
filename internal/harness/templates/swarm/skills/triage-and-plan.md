# Skill: Backlog Triage, Roadmap Ideation & Execution Wave Planning

Use this skill when running `/swarm-project-plan` (in Claude Code or OpenCode) or when prompted directly in Codex or `omp` to turn a repository-wide idea into a reviewed roadmap file and native Epic hierarchy, or to triage open issues with `swarm repo plan`.

- **Scope boundary**: `swarm repo plan` and `/swarm-project-plan` manage repository-wide roadmaps and issue backlogs. Per-issue implementation planning for a claimed task uses `/swarm-plan` (`swarm issue work` / `swarm issue stage`) to write `.swarm/active/PLAN.md` and `TEST_PLAN.md` (and never overrides any harness's built-in `/plan`).

## 1. Interactive Roadmap Authoring & Epic Publication (`swarm repo plan --from`)

1. **Ask for the Idea & Outcome**: Clarify the problem to solve and desired user/developer outcome.
2. **Inspect Repository Context**: Read `.swarm/goals.md`, `.swarm/rules/`, current implementation files, and open tracker issues (`swarm repo plan --scan`).
3. **Propose Independent Tasks & Explicit Dependencies**: Break the work into self-contained `## <title>` tasks. Only add `depends on: <earlier-step-number>` when a task genuinely requires an earlier step's code; keep independent tasks parallelizable. Ask the user for corrections before writing.
4. **Write a Repo-Local Plan File**: Save the plan to a user-chosen repo-local path without overwriting an existing file unless instructed. Format each task with:
   - `## <title>`
   - `### Objective`
   - `### Constraints`
   - `### Acceptance Criteria` with observable `- [ ]` checkboxes
   - Optional `depends on: <earlier-step-number>`
5. **Preview Before Publishing**: Show the saved file and run `swarm repo plan --from <file> --backend github` to preview the Epic and `Step N` child tasks.
6. **Publish on Explicit Maintainer Instruction**: Run `swarm repo plan --from <file> --apply --backend github` only when a maintainer explicitly asks to publish. Show the unlabeled parent Epic (`type: Epic`) and draft child WorkUnit links (`swarm:draft`), and recommend selective `swarm issue approve <N>` on child tasks (never approve or execute the Epic issue, and do not pass `--ready` automatically).

## 2. Issue Types & Triage Label Taxonomy (`swarm repo plan --scan`)

- **Native `Epic` issue type**: Non-executable planning container holding the roadmap document and linked to child WorkUnits via native sub-issues. Epics carry no readiness label and are skipped by `swarm repo plan --scan` and worker queues.
- `swarm:community`: Community bug report, idea, or feedback intake. The label can remain after conversion; a valid, separately approved contract determines readiness for work.
- `swarm:draft`: Valid structured `WorkUnit` awaiting maintainer review and `swarm issue approve <N>`.
- `swarm:needs-scope`: Raw or imported issue missing structured `Objective`, `Constraints`, or `Acceptance Criteria` sections.
- `swarm:blocked`: Issue waiting on open `depends_on` prerequisites or protected-path policy review.
- `swarm:ready`: Maintainer-approved immutable `WorkUnit` ready for agents to claim (`swarm issue claim --next` or `swarm issue work --next`).

## 3. Decomposition & Sense-Making Checklist

1. Verify each proposed or open issue aligns with `.swarm/goals.md` and `.swarm/rules/`.
2. Identify concrete file or package touchpoints for each issue.
3. Use `depends on: <step>` for true prerequisite ordering; treat overlapping touchpoints in `Execution Waves` as scheduling advice.
4. Flag any issue that touches protected paths without `allow_harness_changes: true`.
5. Note advisory size, uncertainty, and risk dimensions (`swarm repo plan --scan` suggests them). Identify touchpoints and open decisions; use a small probe task for unknown answers and a minimal mitigation task when users are affected now. Only maintainer-set dimensions on approved WorkUnits select execution profiles.
6. Work the scan's **Backlog at a Glance** lanes top to bottom. Suggest closing duplicates and quiet issues to the maintainer, but never close, relabel, or comment on issues yourself.
7. Write acceptance criteria someone can check: a command, a test, or an observable behavior. Words like "improve" or "cleaner" alone usually mean the task needs more scoping.
8. Give each task one outcome. Say what is in scope and what is out of scope, and name the backends, harnesses, and platforms it covers instead of writing "every backend".
9. Keep acceptance criteria checkable inside the agent sandbox or by CI. Checks that need an environment agents can't reach, such as a container image, another OS, or a live GitHub flow, belong in the PR review checklist or a follow-up task. Probes are their own investigation task, never a step that blocks another task's implementation. Nice-to-haves become follow-up tasks.

## 4. Individual Intake Conversion and Bulk Normalization

For one intake issue, read its report and prepare an explicit objective, constraints and observable acceptance criteria with the maintainer. Preview using real scoped fields:

```bash
swarm issue convert 123 --objective 'Return an empty result for an empty query' \
  --constraint 'Keep the existing search API' \
  --acceptance 'An empty query returns an empty result without an error' --dry-run --json
```

Only on explicit maintainer instruction, repeat the reviewed command without `--dry-run` to update the existing issue into a draft. Conversion preserves its number, URL and original report; a linked PR closes that same issue on merge. Run `swarm issue lint 123`, request a separate maintainer decision to run `swarm issue approve 123`, then start `swarm issue work 123` when authorized. Never approve or start implementation automatically. Existing WorkUnits use `swarm issue edit`; follow status/recovery guidance for issues with recorded coordination state.

For the whole backlog, preview `swarm repo plan --scan --backend github`. On explicit maintainer instruction apply suggested normalization with `swarm repo plan --scan --apply --normalize --backend github`. Review each resulting draft's requirements before separate approval. Do not add `--ready` automatically. Individual conversion accepts prepared fields; backlog normalization suggests them across multiple issues.
