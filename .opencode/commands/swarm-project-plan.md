---
description: Collaboratively turn a repository-wide feature idea into a reviewed roadmap file, preview Step N tasks, and publish a native Epic with draft child WorkUnits
---

Read `.swarm/goals.md`, `.swarm/SKILLS.md`, `.swarm/skills/triage-and-plan.md`, `.swarm/project.md`, `.swarm/architecture.md`, and `.swarm/rules/`.

> Note: `/swarm-project-plan` is for repository-wide roadmap ideation and backlog triage (`swarm repo plan`). For a single claimed WorkUnit's `.swarm/active/PLAN.md` and `TEST_PLAN.md`, use `/swarm-plan`.

## 1. Interactive Idea-to-Roadmap Workflow (`swarm repo plan --from`)

1. **Clarify the Goal**: Ask for the feature idea, problem statement, and desired outcome if not already specified.
2. **Inspect Context & Open Backlog**: Read `.swarm/goals.md`, `.swarm/rules/`, relevant implementation packages, and open tracker issues (`swarm repo plan --scan`) so proposed tasks fit the architecture and avoid duplicate work.
3. **Draft Independent Tasks & Explicit Dependencies**: Propose a breakdown of small, independently verifiable tasks, calling out only true ordering prerequisites (`depends on: <earlier-step-number>`), and ask for corrections before writing. Give each task one outcome, say what is out of scope, name the backends or platforms it covers, and keep its acceptance criteria checkable in the agent sandbox or CI. Move nice-to-haves into their own tasks.
4. **Write a Repo-Local Plan File**: Ask the user which repo-local path to write (for example `ROADMAP.md` or `plans/<feature>.md`). Do not overwrite an existing file unless the user explicitly instructs you to update it. Format the document with a top-level `# <Epic Title>` heading and one `## <Task Title>` section per child WorkUnit:
   ```markdown
   # <Epic Title>

   ## <Task 1 Title>
   ### Objective
   <Clear single-purpose objective>

   ### Constraints
   - <Architectural or scope boundary>
   - Out of scope: <related work left for a follow-up task>

   ### Acceptance Criteria
   - [ ] <Observable, testable requirement>

   ## <Task 2 Title>
   ### Objective
   <Clear single-purpose objective>

   ### Constraints
   - <Architectural or scope boundary>

   ### Acceptance Criteria
   - [ ] <Observable, testable requirement>
   depends on: 1
   ```
5. **Show the Plan & Preview (`Step N`)**: Show the saved file and run the read-only preview:
   ```bash
   swarm repo plan --from <file> --backend github
   ```
6. **Publish Only on Explicit Maintainer Instruction**: Wait for the maintainer to confirm publishing, then run:
   ```bash
   swarm repo plan --from <file> --apply --backend github
   ```
   Show the created unlabeled Epic (`type: Epic`) and the linked `swarm:draft` child WorkUnit issues. Recommend selective approval for the child issues ready to execute:
   ```bash
   swarm issue approve <child-issue-number> --backend github
   ```
   Never run `swarm issue approve` or `swarm issue work` on the Epic issue itself, and do not pass `--ready` automatically unless the maintainer explicitly asks for immediate approval.

## 2. Backlog Sense-Making & Community Intake Triage (`swarm repo plan --scan`)

- **Preview open backlog & `swarm:community` intake alignment**:
  ```bash
  swarm repo plan --scan --backend github
  ```
- **Apply triage labels (`swarm:ready`, `swarm:draft`, `swarm:needs-scope`, `swarm:blocked`) and optionally normalize unstructured issues into draft WorkUnits**:
  ```bash
  swarm repo plan --scan --apply --normalize --backend github
  ```
