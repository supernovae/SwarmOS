# Changelog

Notable changes to SwarmOS, newest first. Upgrade `swarm` and rerun `swarm repo init` after a release to pick up regenerated workflows and templates.

## Unreleased

## v0.8.0

v0.8.0 is about getting work across the line without hand repairs. Rebases resolve and repair routine conflicts, approvals follow main forward when nothing that matters changed, abandoned claims and checkpoints have maintainer commands to clear them, and validation fails fast with the failing tests in view. Fork contributors can keep the `origin`/`upstream` layout `gh` creates, and projects that prefer a tidy branch list can move SwarmOS's ledger refs under `refs/swarm/`. Upgrade `swarm` and rerun `swarm repo init` to pick up the regenerated workflows and templates.

- Show bounded, terminal-safe failed-check output and Go failing test names during repository, issue, and PR validation and submission. JSON failed-check outcomes include `output_tail` and `failing_tests`.

- `swarm issue submit` reports target-branch drift and the behind count before publishing. Conflicting submissions still open a draft PR, with conflicted paths and a numbered `swarm pr rebase <PR> --resolve` recovery command in the terminal and PR description. Unavailable freshness checks are skipped, and JSON output includes `base_freshness`.

- Fresh same-repository claims start at the current default-branch tip when task, committed policy tree and ancestry checks pass. Existing plan evidence requires authenticated review carry; human gates require explicit approval of a moved base. Queued work reports and skips fresh candidates needing approval, including stale task approval at an unchanged tip.

- Extend `swarm pr rebase --resolve` with recorded per-conflict micro-plans, trusted post-rebase validation, and bounded local repair (default two attempts, configurable with `lease.rebase_repair_limit`). Successful runs print the explicit submit command. Eligible maintainers can re-pin unchanged task/policy approval automatically; policy changes require explicit approval before unchanged plans receive a fresh agent review. Fork workers retain explicit maintainer approval and review steps.

- `swarm pr rebase <PR> --resolve` lets the configured developer agent resolve routine rebase conflicts. At each conflicted commit the agent gets one turn on a disposable workspace holding only the conflicted files, both sides, the WorkUnit, and the approved plan. SwarmOS accepts edits limited to those files with no remaining markers, stages them, and continues; anything else leaves the rebase paused as before. Resolutions appear in `pr status` and the PR's rebase context, and `pr work` and `pr stage` now point at a paused rebase's recovery commands.

- Feed current-head failing and cancelled PR checks into `swarm pr work` through quoted `.swarm/active/CI.md` evidence, including annotations and bounded GitHub Actions log tails. `pr status` notes when evidence will be available.

- Fail no-op and explicitly blocked implementations before testing, and refuse empty issue submissions with a recovery command.

- Add `swarm issue work --instructions` and record delivered operator guidance in role reports, status, and PR review evidence.

- Replan after requested changes, mark earlier role attempts as superseded, and keep unmerged lessons out of agent system prompts.

- `swarm issue edit` replaces selected WorkUnit sections on GitHub and local backends, previews diffs and approval impact, lints before writing, and blocks active claims. Changed contracts require separate approval.

- Protected paths now cover the files that become part of agent instructions: `.swarm/project.md`, `.swarm/architecture.md`, `.swarm/conventions.md`, `.claude/commands/**`, `.opencode/commands/**`, `AGENTS.md`, and `CLAUDE.md`. New repositories get them from `swarm repo init`; existing repositories can add them to `.swarm/validation/required-checks.yaml`. Agent work that needs to edit them sets `allow_harness_changes: true` on its WorkUnit.

- Maintainer pull requests can change protected policy without failing CI. `swarm repo validate --protected-paths report` lists the changed protected files, notes when `.swarm/` policy changed, and still runs every required check; the generated workflow uses it only for maintainers' own branches, keeping agent branches, forks, and other authors on strict enforcement. SwarmOS's own CI now runs this check with a validator built from the trusted base.

- Agent run budgets now support approved global and per-role durations, with a 60-minute default and a warning before the deadline. Interactive native sessions exclude waits for operator input and have a four-hour wall cap; native Codex and Bubblewrap preserve bounded timeout patches for manual recovery. OCI uses wall time and reports that no timeout patch is available. Required-check deadlines are unchanged.

- Re-approving an issue on a newer base drops a checkpoint whose commits are already contained in that base. Resuming moves clean local work forward and retains the original remote head as the next publication fence; checkpoints with unique commits remain preserved.

- Approved plans can carry across descendant PR rebases when the WorkUnit and committed policy tree are unchanged. Fresh validation and explicit submission remain required; status explains when a new plan cycle is needed.

- Rebase status and PR updates include heads, bases, conflict paths, and Git patch identity. Claim and interrupted-rebase errors include recovery commands, and Git errors render carriage returns cleanly.

- Contributors whose `origin` is their personal fork and whose `upstream` is the project (the layout `gh repo clone <fork>` and `gh repo fork --remote` create) can now use swarm without renaming remotes. Pass `--upstream-remote upstream`, or save it once with `swarm --upstream-remote upstream node init` (the `upstream_remote` node profile setting). swarm then reads and writes coordination state, approved bases, and same-repository task branches on that remote, and with `--fork` uses `origin` as the fork remote unless `--fork-remote` names another one. Before any write, swarm checks that `origin` is a repository you own and can push to and that it is a different repository from the project. If a check fails, the error names the remote and the fix. Without the setting nothing changes: `origin` is the project and the fork remote defaults to `fork`.

- `swarm issue cleanup --apply` retires clean merged issue and PR worktrees and local code branches, with per-resource results and exact-tip branch deletion. Local backends need no origin; dirty or unsafe worktrees and their branches remain available.

- `swarm issue work`, `swarm issue validate`, and `swarm pr validate` first run `go test` on the packages changed since the approved base, and their in-module reverse dependencies, before a required `go test ./...` check. A failure there stops validation early and names the packages. A pass always continues to the unchanged full check, which decides the result. Submit, staging, `repo validate`, and CI are unchanged.

- `swarm issue work`, `issue submit`, `pr work`, `pr rebase`, and `pr submit` report why they stopped. Their `--json` failures add an advisory `next_action` (`reason`, literal `argv`, `human_required`), and the process exits with `3` for `changes_requested`, `4` for `needs_approval`, or `5` for `conflict`. Other failures still exit `1`, and successful runs, including human-gate pauses, exit `0` without `next_action`.

- **`**` in protected paths matches at any depth:** `**/.gitattributes` now also protects `.gitattributes` at the root and in nested directories, and `a/**/b.yaml` matches `a/b.yaml` and `a/x/y/b.yaml`. Previously `**` matched exactly one directory there, so those files could change without tripping the protected-path check. Trailing `dir/**` and ordinary patterns match as before. A malformed pattern in the trusted policy, such as `[`, now fails validation with an error naming it instead of silently matching nothing.

- `swarm pr status` now lists open code-scanning alerts (for example, from CodeQL) on the PR's merge ref with rule ID, severity, `path:line`, and message, and `--json` includes them under `result.code_scanning`. When checks fail, `swarm pr work` adds the same alerts to the developer's `.swarm/active/CI.md`. Alert text is escaped and size-limited, at most 50 alerts are shown with a count of the rest, and a token without code-scanning access reports `unavailable (no access or not enabled)` without failing either command. Check metadata in `CI.md` now reserves 512 bytes for these notices.

- Use independent changelog fragments and `make changelog VERSION=x.y.z` to keep parallel changes from conflicting.

- `swarm issue cleanup --apply` now retires original issue worktrees and local branches at recorded checkpoint ancestors of a verified merged PR tip, including PRs revised after submission.

- `swarm issue cleanup --all-merged` previews every local issue and PR worktree with a reason for each resource, using one pull request listing. With `--apply` it retires eligible resources one issue at a time through the single-issue checks, keeps going after a failed issue, and prints retired, retained, and failed counts per issue and in total. `--json` prints one summary.

- Save a bounded, redacted, and escaped tail of a failed agent run's output to a private local log under the user state directory. `swarm issue work`, `swarm pr work`, and `swarm pr rebase --resolve` failures, including `--json` errors, report only the log's path and size. Logs older than seven days or beyond the newest 20 are pruned.

- `swarm issue cleanup --apply` now retires original issue worktrees and local branches after a PR was rebased with `swarm pr rebase`, including rebases that resolved conflicts and changed patch identity, when the worktree is at the old head of a coordinator-verified rebase chain that reaches the merged tip. A worktree at an earlier checkpoint that is only an ancestor of a rebase's old head is now retained.

- `swarm pr rebase` and `swarm pr status` now warn with `no longer changed: <path>` when a rebase, conflict resolution, or repair leaves a path the PR used to change identical to the new base, and record the paths as `last_rebase.dropped_paths` in the rebase evidence.

- `swarm pr submit` now refuses a PR revision that has an approved plan but no new commits, for example after a failed implementation step, and keeps the plan and review. Run `swarm pr work <PR> --phase implement` (`swarm --fork pr work <PR> --phase implement` for contributors) to resume without re-planning.

- `swarm issue work` and `swarm issue claim` now fast-forward an untouched task worktree to the newly approved base after re-approval, instead of failing with `expected checkpoint`. The branch moves with a compare-and-swap. The plan, test plan and review come from authoritative state. Worktrees with commits or edits are still refused, and the error now names the recovery commands.

- When a failed check's output is too long to show in full, validation now prints each Go `--- FAIL:` line and its indented messages first, followed by a shorter tail of the output. The text and `--json` `output_tail` keep the same size limits and show a `[swarm: output truncated]` marker where output is cut. Previously, unrelated output printed after the failures could push the failure messages out of the tail.

- `swarm repo init` and task worktrees support an `AGENTS.md` or `CLAUDE.md` that is a relative symlink to a regular file inside the repository, such as `AGENTS.md -> CLAUDE.md`. Init writes its marked SwarmOS section into the linked file once and keeps the link; task worktrees add and remove a single pointer block. Links that leave the repository or lead to a directory, special file, or missing file are refused before any write.

- `swarm --fork issue status` works again against an upstream coordinator running a swarm release from before claim budgets. Status now prints `claim budget: unavailable from upstream coordinator` and the rest of the task details, and `--json` reports `"claim_budget_unavailable": true` with no `claim_budget` instead of failing with `upstream coordinator did not supply claim budget`. Fork-mode work against such a coordinator times its wind-down from the current lease expiry and keeps a reserve of a third of the lease duration. The coordinator still decides every renewal and claim check. When the coordinator does send a claim budget, output and behavior are unchanged.

- In `--fork` mode, a GitHub server error (5xx) or timeout while posting a coordinator request no longer leads to a duplicate request. Each request comment now carries a random request nonce in a hidden marker on its summary line, outside the request details. After an ambiguous post, swarm looks through the issue's recent comments for that nonce and continues with the comment it finds. If the comment is not there, swarm posts it once more. It never posts a third time, and if it cannot tell whether the request was posted, it says so and gives the nonce. Comment reads that fail with a server error or timeout are retried briefly, and an error that persists names the request. The nonce is only used to match the comment; coordinators ignore it and still check every request against GitHub and coordination state. The request details are unchanged, so released Fork Coordinator workflows keep accepting these requests without an update. If a coordinator ever rejects a request it cannot read, swarm now reports that rejection right away instead of waiting for the timeout.

- `swarm issue status` and `swarm pr status` no longer report a task worktree as dirty when its only change is the SwarmOS pointer block in `AGENTS.md` or `CLAUDE.md` (including a generated `CLAUDE.md` and linked instruction files). Any other edit, including text outside the block, still shows as dirty.

- Maintainers can now discard an abandoned task checkpoint with `swarm issue discard-checkpoint <issue> --expected-head <sha>`. Use it when an unsubmitted checkpoint should not be continued, for example after the WorkUnit was edited and re-approved on a base the checkpoint does not descend from. Before, `swarm issue claim` and `swarm issue work` failed in that case, and the only fix was editing the ledger by hand. The command needs the same authority as `swarm issue approve`. It is refused while the issue is claimed, has a pull request, or holds a fork contributor's checkpoint. The discarded commit stays reachable under `refs/heads/swarm-checkpoints/issue-<n>/<sha>`, and `swarm issue status` lists it. The next claim starts on a fresh branch at the approved base and plans from scratch. A local worktree still holding the old attempt is moved aside unchanged. When provisioning hits a checkpoint that does not descend from the approved base, the error now names this command. Works on the GitHub and local backends in same-repository mode.

- Coordinator commands run from inside a task or PR worktree (by `cd` or `--root`) now use the main checkout, with a one-line notice, instead of creating nested worktrees under `.swarm/worktrees/issue-N/.swarm/worktrees`. When the main checkout cannot be verified, the command refuses before writing anything and asks you to cd to the main checkout or pass `--root`.

- Reject developer and tester changes to protected files and approved plan inputs before importing returned work, leaving the worktree unchanged when a run is rejected.

- **Ledger refs out of the branch list:** a new `ledger_namespace` setting in `.swarm/config.yaml` chooses where issue state, checkpoint retention, and fork contributor indexes live. `branch` keeps `refs/heads/swarm-state/`, `swarm-checkpoints/`, and `swarm-contributors/`, and is what existing configurations without the key get. `hidden` uses `refs/swarm/state/`, `checkpoints/`, and `contributors/`, which stay out of the branch list and the "Compare & pull request" banner, but GitHub rulesets cannot protect them. `swarm repo init` writes `hidden` for new configurations. SwarmOS reads the setting only from the upstream default branch, and stops with an error naming both refs when a ledger ref exists in the namespace it did not select. Task branches stay branches. `swarm repo migrate-refs` moves an existing repository's ledgers in one atomic, leased push; see [Ledger ref namespaces](docs/GITHUB.md#ledger-ref-namespaces).

- Add `swarm issue revoke <issue> --reason <text>` so maintainers with `approve-task` permission can end an abandoned claim on the github backend. Revoke records the maintainer and reason in authoritative state, removes fork claims from the contributor's active-claim index in the same atomic push, keeps approval, artifacts, and checkpoints, and fences the previous holder's writes.

- Same-repository claims and renewals are now capped at the approved `lease.duration`, as in fork mode; a longer `--duration` is clamped and shorter requests are kept.

- `swarm issue approve` prints the `swarm issue lint` shape and hints before approving, and planners see the approved total claim budget. A plan that contains the line `Split recommendation: required` stops `swarm issue work`, `swarm pr work`, and `swarm node run` after planning, without review, implementation, or a FAIL verdict, and prints the `swarm issue decompose` command for the linked issue.

- Catch lint and format failures before CI. `swarm repo init` adds a gofmt `fmt` required check for Go repositories, and `swarm repo doctor` warns, with a suggested `required-checks.yaml` entry, when a GitHub workflow runs gofmt, `go vet`, staticcheck, golangci-lint, eslint, `prettier --check`, ruff, `black --check`, `cargo fmt --check`, or `cargo clippy` but no required check does. The warnings never fail doctor or edit policy. The shipped developer prompt asks agents to run every required check before finishing. Host-executed required checks now share a persistent, SwarmOS-owned Go build cache per repository under the user cache directory, separate from your own `GOCACHE` and never mounted into sandboxes.

- `swarm issue submit` and `swarm pr submit` reuse a passing validation from the same claim when the exact tree, base, policy, claim, and swarm build are unchanged, printing its age and short tree SHA and noting the reuse in the PR description; any mismatch, a prior failure, or a `dev` build validates as before.

- Fix `swarm` heartbeats reporting a deliberate stop as a failure when the stop landed while the heartbeat was loading claim state.

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
