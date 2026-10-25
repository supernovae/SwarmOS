# Changelog

Notable changes to SwarmOS, newest first. Upgrade `swarm` and rerun `swarm repo init` after a release to pick up regenerated workflows and templates.

## Unreleased

- `swarm issue edit` replaces selected WorkUnit sections on GitHub and local backends, previews diffs and approval impact, lints before writing, and blocks active claims. Changed contracts require separate approval.

- Protected paths now cover the files that become part of agent instructions: `.swarm/project.md`, `.swarm/architecture.md`, `.swarm/conventions.md`, `.claude/commands/**`, `.opencode/commands/**`, `AGENTS.md`, and `CLAUDE.md`. New repositories get them from `swarm repo init`; existing repositories can add them to `.swarm/validation/required-checks.yaml`. Agent work that needs to edit them sets `allow_harness_changes: true` on its WorkUnit.
- Maintainer pull requests can change protected policy without failing CI. `swarm repo validate --protected-paths report` lists the changed protected files, notes when `.swarm/` policy changed, and still runs every required check; the generated workflow uses it only for maintainers' own branches, keeping agent branches, forks, and other authors on strict enforcement. SwarmOS's own CI now runs this check with a validator built from the trusted base.
- Agent run budgets now support approved global and per-role durations, with a 60-minute default and a warning before the deadline. Interactive native sessions exclude waits for operator input and have a four-hour wall cap; native Codex and Bubblewrap preserve bounded timeout patches for manual recovery. OCI uses wall time and reports that no timeout patch is available. Required-check deadlines are unchanged.
- Re-approving an issue on a newer base drops a checkpoint whose commits are already contained in that base. Resuming moves clean local work forward and retains the original remote head as the next publication fence; checkpoints with unique commits remain preserved.

- Approved plans can carry across descendant PR rebases when the WorkUnit and committed policy tree are unchanged. Fresh validation and explicit submission remain required; status explains when a new plan cycle is needed.
- Rebase status and PR updates include heads, bases, conflict paths, and Git patch identity. Claim and interrupted-rebase errors include recovery commands, and Git errors render carriage returns cleanly.

## v0.7.0

v0.7.0 is about owning a busy repository: seeing the whole backlog at once, sizing work to fit, and being clear about who reviews what. Upgrade `swarm` and rerun `swarm repo init` to pick up the new rules file and issue form wording.

- **Backlog at a glance:** `swarm repo plan --scan` sorts open issues into lanes (stabilize first, probable duplicates, quiet for a while, ask the author, waiting, ready, needs a decision). `--stale-days` tunes what counts as quiet. Lanes are suggestions; the scan never closes, comments on, or relabels issues because of them. See [Owning a Busy Repository](docs/backlog.md).
- **Open pull requests in the same view:** the scan also lists open PRs in lanes (competing, dependency updates, quiet, ready for review, in progress) and marks which ones belong to a WorkUnit, confirmed against coordination state rather than PR text.
- **Dependency updates without the noise:** PRs from Dependabot and Renovate get their own lane, and a new guide covers grouping updates, why GitHub Actions bumps fail SwarmOS Validation on the protected workflow path, and adopting updates as a WorkUnit.
- **Task shapes:** planner, reviewer, and developer prompts name the work's shape (Routine, Involved, Exploratory, Urgent), and `PLAN.md` opens with what we know, assumptions, and open questions. `repo plan --scan` and `issue lint` suggest a shape from the issue text.
- **Lessons loop:** agents note repository surprises as they work, and reviewers see them in the PR under "What we learned". `swarm repo lessons --apply` collects lessons from merged WorkUnit PRs into `.swarm/rules/lessons.md` for you to review and commit.
- **Name your plan reviewers:** `security.reviewer_agents` pins which configured agents can pass an agent gate, and `security.independent_review` keeps the planner (or the contributor who claimed the task) from approving their own plan. Both load from the approved base, like the rest of `security:`.
- **Reviewers hear about it:** when a plan pauses at a human gate, SwarmOS assigns the `security.reviewer_collaborators` to the issue and mentions them once per plan revision. `swarm repo doctor` shows who can satisfy each gate and warns when a human gate has nobody named.
- **Hosted review bots in `pr status`:** checks show the GitHub App that reported them, and reviews say whether they cover the current head or an earlier commit. A new guide covers working alongside Codex code review, Copilot code review, Cursor Bugbot, and hosted coding agents.
- **Per-harness guides:** [`docs/harness/`](docs/harness/README.md) covers setup, invocation, isolation boundary, and troubleshooting for Codex, Claude Code, OpenCode 2, and OhMyPi.

## v0.6.1

v0.6.1 fixes issues in v0.6.0's fork PR revision and generated CI workflows. Upgrade swarm and rerun `swarm repo init` to pick up the regenerated workflows.

- **Fork PR revisions work end to end:** the coordinator workflow now admits the PR revision requests, PR claims use a configured agent, and later revision rounds catch up to newer PR heads instead of reporting divergence.
- **Generated CI passes on stock runners:** CI runs required checks on the runner itself, validates pull requests against the commit GitHub merged into, and checks default-branch pushes against their own policy, so harness upgrades no longer turn the branch red.
- **Smoother publishing:** `pr submit` works after the target branch advances and waits briefly for GitHub to report a pushed fork head.
- **Safer recovery:** validation ignores SwarmOS's temporary task pointer in protected instruction files, a failed resume keeps its claim, and `repo doctor` checks for a git commit identity before agents run.
- **Readable fork activity:** fork requests and coordinator replies show a one-line summary with the JSON collapsed underneath.
- **`make install`** builds a checkout into `$HOME/go/bin` for local testing.

## v0.6.0

- **Resource-oriented CLI:** Commands are grouped by what they act on—`swarm repo`, `swarm issue`, `swarm pr`, and `swarm node`—so the same verbs (`work`, `stage`, `validate`, `submit`, `status`) mean the same thing everywhere. Every leaf command accepts `--json`.
- **Continue existing PRs:** `swarm pr status|work|stage|rebase|validate|submit <PR>` keeps developing a submitted WorkUnit PR—failing tests, review feedback, rebases—in a separate PR worktree, and publishes only with `pr submit` against the head it started from.
- **Fork contributors revise their own PRs:** With `--fork`, contributors address review feedback on their fork PRs through the upstream fork coordinator, which verifies fork ownership, the published head, validation, and maintainer plan approval before recording each revision. Pushes made outside SwarmOS show as `unverified-head` in `pr status`.
- **`swarm repo init` upgrades cleanly:** Generated workflows pin themselves to the swarm you run, and rerunning init after an upgrade updates untouched generated files while keeping your edits. Init adds its own `swarm-*` workflows and a marked section in an existing `AGENTS.md`, and never edits your other files.
- **Hardened coordination:** Long validations renew their lease, issue commands never guess between worktrees, and PR publication to origin always moves the branch and coordination state together.

## v0.5.0

- **Live work progress:** Headless `swarm issue work` reports stage transitions and a 15-second heartbeat on stderr while keeping stdout and JSON results separate. Use `--progress=false` for quiet runs.
- **Provenance at the terminal:** `swarm issue status <issue>` shows recorded per-role harness, model, sandbox boundary, and artifact digests; `--json` includes structured provenance.
- **Clearer review handoffs:** Generated PRs distinguish automated validation from the maintainer's acceptance checklist, with guidance for reviewing the finished implementation.
- **Automatic version metadata:** `swarm version` reads Go's embedded build information when release metadata is not supplied, replacing the old fixed version in source builds.
- **Nested sandbox compatibility:** Detached sandbox builds work without Git metadata, and regression checks handle restricted nested sandboxes.
