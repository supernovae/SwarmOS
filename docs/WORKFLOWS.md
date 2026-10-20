# Workflows, prompts, and gates

## Prompt recipes, not a workflow graph

`.swarm/workflows/feature.yaml`, `bugfix.yaml`, and `refactor.yaml` name steps and role context. They are fixed **prompt recipes**. The engine does not execute arbitrary YAML edges or automatically create a PR from a `create_pr` step.

The fixed lifecycle is:

```text
plan → publish design + test plan → required review gate → implement → validate → explicit submit
```

This distinction matters when editing workflow YAML: it changes the context and expected artifacts, not the control-flow security model.

## Role outputs

| Role | Reads | Required output |
| --- | --- | --- |
| planner | WorkUnit and repository context | `.swarm/active/PLAN.md`, `TEST_PLAN.md` |
| reviewer | WorkUnit, plan, test plan | `REVIEW.md` beginning with an exact verdict |
| developer | approved plan and test plan | source changes and implementation checkpoint |
| tester | implementation and approved-base validation policy | validation evidence |

The active bundle is generated per task. It contains `WORKUNIT.md`, `SYSTEM_PROMPT.md`, and prior artifacts. Do not reuse artifacts across issue revisions or replace a plan/test plan after it has been approved without obtaining new review.

## Gate behavior

```bash
# Show task, gate, policy, workflow and resolved agents without side effects.
swarm work 42 --dry-run

# Agent gate: run plan then the configured reviewer in headless mode.
swarm work 42 --gate agent --headless

# Human gate: plan then pause; a permitted human decides using the CLI.
swarm work 42 --gate human --phase plan --headless
swarm review 42 --approve --notes "Proceed with the current plan"
swarm work 42 --phase implement --headless
```

The exact effective gate is resolved from task, workflow, and repository policy. A CLI value can only preserve or raise policy; it cannot use `--gate none` to evade a human/agent requirement.

For an agent gate, the reviewer must produce exactly one of:

```text
VERDICT: APPROVED
```

or

```text
VERDICT: CHANGES_REQUESTED
```

Malformed, absent, stale, or rejected verdicts stop before implementation. The recorded approval binds the WorkUnit digest and hashes of both plan files. A human decision is authenticated through tracker authorization and is distinct from an agent-produced review artifact. Human-gated standalone review is a pause, not an agent substitute.

## Harness recipes

The generated slash-command files are instructions for a compatible agent harness:

- `/swarm-plan` produces plan and test-plan artifacts.
- `/swarm-review` audits the current plan and writes a review artifact.
- `/swarm-implement` applies the approved plan.
- `/swarm-validate` runs repository validation.

They do not grant permissions, create transitions, or bypass the CLI/engine state checks. Reviewer use requires the configured isolation boundary; planner, developer, and tester processes remain host processes with restricted credentials, not a general sandbox.

## Failure and recovery

A claim has a unique ID and expiry. The execution heartbeat renews it; if renewal or claim verification fails, the task stops before a later mutation. Resume only with the persisted claim token, never by assuming an active claim with the same username.

Plans, artifacts, branch, base SHA, and checkpoint head are recorded in authoritative state. A fresh clone can provision the recorded branch/head only after verification. If a local worktree is missing, SwarmOS must not reset a surviving branch. If a review, agent run, validation, or checkpoint fails, inspect the task with `swarm status <issue>`, correct the cause, and resume/reclaim according to the live state rather than editing coordination comments.

`swarm submit` is a separate transition. It verifies the current gate, checkpoints the branch, validates that exact clean head against the approved base, records validation/checkpoint/submission state through the fenced Git-CAS path, and reconciles an existing Draft PR on retry. An earlier validation run is not evidence for changed code, and a Draft PR does not mark acceptance criteria complete without criterion-specific evidence.

Related: [maintainer setup](project.md), [developer operations](developer.md), [Git-CAS architecture](ARCHITECTURE.md).
