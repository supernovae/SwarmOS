# Developer, Team & Worker Guide

This guide covers day-to-day SwarmOS workflows—whether you are an individual developer, an open-source contributor donating agent compute to a project's ready queue, or an engineering team running internal worker pools across private GitHub repositories. Pass `--backend github` when working against GitHub Issues, or `--backend local` when running an offline local fixture.

---

## 1. Everyday Task Flow

```bash
# Inspect the resolved agents, gate, and base commit without claiming or running processes
swarm issue work 42 --dry-run --backend github

# Reserve an approved task and provision .swarm/worktrees/issue-42
swarm issue claim 42 --backend github

# Run (or resume) planning, review gate, implementation, and validation
swarm issue work 42 --backend github

# Run validation checks directly or submit the validated branch as a Draft PR
swarm issue validate 42 --backend github
swarm issue submit 42 --backend github
```

Developer and tester results are checked before import. If a run changes protected files or approved inputs in `.swarm/active/`, the error and `swarm issue status 42` list every rejected path. The returned changes are discarded and the worktree stays as it was at launch. Retry with `swarm issue work 42 --phase implement` after addressing the cause. Developers may write `IMPLEMENTATION.md` and `LESSONS.md` in the active bundle; testers may write `VALIDATION.md` and `LESSONS.md`. The coordinator restores approved plan inputs before the tester starts.

Helpful CLI conventions:

- **Picking up the next task**: Use `swarm issue claim --next` to reserve the next eligible `swarm:ready` issue, or `swarm issue work --next` to claim and execute it immediately. Add `--all` with `--next` (or `swarm issue claim --next --all`) to scan across all repositories registered in `~/.config/swarm/repos.yaml`.
- **Interactive & harness-driven staging (`swarm issue stage`)**: Working inside an interactive coding harness without spawning nested subprocesses? Run `swarm issue claim <issue>`, write `.swarm/active/PLAN.md` and `TEST_PLAN.md`, and stage each phase directly with `swarm issue stage <issue> --phase plan --harness <name> --model <name>` and `swarm issue stage <issue> --phase implement --harness <name> --model <name>`.
- **Token auth without `gh` CLI**: Set `SWARM_GITHUB_TOKEN` (or `GH_TOKEN` / `GITHUB_TOKEN`) to authenticate directly with GitHub REST and Git HTTPS remotes on headless machines without installing `gh`.
- **Secret-manager tokens**: Set `SWARM_GITHUB_TOKEN_COMMAND` (for example `op read op://Private/GitHub/swarm-token` or `security find-generic-password -s swarm-github -w`) so the token stays in 1Password, Bitwarden, Keychain, or libsecret instead of your shell. See [Keep tokens out of your shell](GITHUB.md#keep-tokens-out-of-your-shell-with-a-secret-manager).
- **Headless vs. interactive**: `swarm issue work` runs non-interactively (`--headless=true`) by default. Pass `--headless=false` to follow the agent's work and add instructions between turns.
- **Structured JSON output**: Use `--json` on a leaf that supports it, such as `swarm issue claim --next --json`, `swarm issue work 42 --json`, `swarm issue status 42 --json`, or `swarm repo validate --base <trusted-full-commit-sha> --json`. Bare `swarm issue` and `swarm repo` are command groups, not JSON-producing operations.

Automatic selection tries the lowest issue number first within each repository. Ready candidates sharing a native parent Epic take that epic's sub-issue order within the positions those siblings occupy. For example, issues 10, 20, 30 with siblings ordered 30, 10 are tried as 30, 20, 10: an earlier sibling can move ahead of a lower-numbered standalone issue.

Dependencies, capabilities, gates, and leases still determine eligibility; an ineligible sibling is skipped. With `--all`, repository traversal order stays unchanged.

If ordering metadata cannot be read, SwarmOS warns on stderr and keeps numeric order for the affected siblings. Existing local links use numeric order; new local links retain their append order.

### Revise a WorkUnit

Use `swarm issue edit` to revise a WorkUnit with a lint check and a clear view of its approval impact. Maintainers and repository collaborators with write access can use it on the GitHub backend; the local backend supports the same flow.

```bash
swarm issue edit 42 --objective 'Add structured WorkUnit editing' --dry-run --backend github
swarm issue edit 42 --objective 'Add structured WorkUnit editing' --backend github
swarm issue approve 42 --backend github
```

`--title`, `--objective`, `--constraint`, `--acceptance`, `--depends-on`, `--gate`, and `--workflow` replace their named fields. Omitted sections keep their original bytes. Repeat `--constraint` or `--acceptance` to supply the complete replacement list. Use `--depends-on=` to clear prerequisites. The edited contract must pass the same validation as `swarm issue lint`.

```bash
swarm issue edit 42 --constraint 'Keep claims fenced' --constraint 'Preserve existing formats' --dry-run
swarm issue edit 42 --depends-on= --dry-run --json --backend local
```

`--dry-run` shows the body diff, title change, lint result, and approval and claim impact without writing. Add `--json` for structured results. An active claim blocks editing and identifies its owner and expiry; coordinate with the worker and use `swarm issue release 42` before revising the contract.

Changing an approved task's parsed content moves its ready label to `swarm:draft` and requires a separate `swarm issue approve 42`. Formatting changes that preserve the task digest retain approval and labels. Editing never grants approval.

Inline Markdown and fenced YAML WorkUnits are supported. For a `swarm-workunit-path` reference, edit the referenced repository file instead. Ambiguous duplicate sections and YAML anchors or aliases must be simplified before editing. Epics use their own planning workflow and cannot be edited with this command. Fork contributors work with an upstream maintainer to revise the contract.

The command re-reads the issue and claim state before writing and refuses a detected concurrent edit. GitHub's issue PATCH is unconditional, leaving a short interval between that final read and the write; coordinate edits with other maintainers. If a body, title, or label request fails after an earlier request succeeds, the error identifies completed steps so you can inspect the issue and retry.

### After a pull request is merged or closed

Keep the issue's recorded submission connected to its GitHub outcome with an explicit reconciliation. Repository operators with write access can refresh the PR outcome and repair the `swarm:pr-open` label and audit comment:

```bash
swarm issue reconcile 42 --backend github
swarm issue status 42 --backend github
swarm issue cleanup 42 --backend github
```

Reconciliation distinguishes open, merged, and closed without merge; closing an issue alone never marks its PR merged. Status commands remain read-only. The cleanup preview reports the remote branch, local branches, managed worktrees, and retained state ledgers separately.

Run `swarm issue cleanup 42 --apply --backend github` to retire eligible same-repository task worktrees and code branches. Cleanup requires each worktree's HEAD to equal the recorded submitted head, which must match the merged PR's final head. This is the submitted tip rather than the merge commit, including for squash and rebase merges. An active attempt, pending revision, incomplete merge evidence, or open PR using either task branch blocks the operation. Fork branches remain with their owners.

Stop editors, agents, and other writers in completed checkouts before applying cleanup. Tracked changes, unexpected untracked or ignored files, a moved HEAD, or unsafe worktree paths retain the affected worktree and its branch with a reason. Only untracked artifacts under `.swarm/active/` and `.swarm/active-output/` are disposable. Cleanup checks again before non-forced Git removal; these checks cannot prevent an outside process from writing after inspection. Independent clean resources can still retire, and text and JSON output report each result.

Remote deletion and its state progress record use one atomic Git push with exact expected heads. Local branch deletion uses the original expected tip in a Git compare-and-swap; a moved branch stays intact. Rerun cleanup to handle remaining resources, including after the remote branch is already absent. State and contributor ledgers remain available for status and provenance inspection.

With `--backend local`, local resources are scoped to the current repository and remote branch verification is reported as not applicable. Local open PRs with no repository identity conflict by branch name against both the issue branch and `swarm/pr-<N>`; unrelated branches do not block cleanup. The local backend consumes recorded merged outcomes but currently has no offline merge or mark-merged command. The GitHub backend continues to require matching upstream, origin, and PR repository identities.

If an interrupted cleanup leaves a reservation, run `swarm issue cleanup 42 --recover-reservation --backend github` to fence that operation and preview again, then rerun cleanup explicitly. Recovery retains the operation's audit history. GitHub cannot transact PR creation together with Git ref deletion; cleanup rechecks open PRs immediately before deletion, but a PR created after that check can race with it.

### Resume after a claim-budget pause

Long runs save their progress before running out of claim time. The approved lease policy sets an absolute ceiling of `lease.duration × (1 + lease.max_renewals)` from acquisition. Heartbeats count as renewals, and early renewals can leave the current claim expiring before that ceiling. `swarm issue status 42` shows renewals used and remaining, the nominal allowance left, and the current expiry.

Before starting an agent stage, SwarmOS reserves one third of the approved lease duration for shutdown and checkpointing. Once the final renewal is used, reaching that reserve stops the agent. Developer and tester edits are safely imported, checkpointed, and handed back to the implementation phase while the claim is valid. Planning and review restart their pending phase. The pause prints the command to continue with a new claim:

```bash
swarm issue work 42 --phase implement
swarm pr work 123 --phase implement
```

Issue checkpoints use the existing fenced publication path. PR revision checkpoints remain local until explicit `swarm pr submit`. A paused tester resumes validation directly when the saved head still has matching implementation evidence; changed code follows the implementation path again. Node workers report a budget pause without counting a failure or automatically submitting it. If shutdown or publication outlasts the lease, claim fencing stops the operation and reports an error.

This automatic pause is separate from an agent execution timeout, whose recovery patch requires inspection before use.

### Choose how you work

Start interactively when you want to stay close to the changes. Move repeatable tasks to unattended workers once the tools and permissions are set up.

| Working style | How to use it |
| :--- | :--- |
| **Pair with an agent** | Run `swarm issue work 42 --headless=false`. Native Codex streams its work, then accepts a follow-up instruction; press Enter to finish the phase. |
| **Stay in your current agent session** | Use `swarm issue claim <issue>` or `swarm issue claim --next`, work in the assigned worktree, and record each phase with `swarm issue stage`. Your existing session keeps its own permissions. |
| **Let a worker pick up tasks** | Run `swarm issue work --next` for one task or `swarm node run --watch` for a continuing queue. Both use the same numeric and epic sibling ordering as `claim --next` and run headlessly; configure the harness to complete allowed work without waiting for keyboard input. |

Native Codex uses the same bounded permissions for pairing and unattended work. Allowed commands run without approval prompts; requests for broader access are denied. Each turn uses an ephemeral session, with task files carrying progress forward. For an existing agent session or `trusted-local` setup, keep the harness's approval prompts enabled when pairing. Reserve full-access or “YOLO” settings for disposable workers with independently enforced isolation; CI alone does not provide that boundary.

You can still pause automated work for a maintainer's plan review with `--gate human`. That checkpoint is separate from the harness's per-command permission prompts.

### Choose where agents run

SwarmOS uses your agent's normal login and security controls. On **Linux**, Codex uses its native permission profiles and sandbox. Claude Code, OpenCode 2, and OhMyPi keep their normal provider sessions and use a restricted tool executor for workspace editing and commands. Install your harness, sign in normally, then run `swarm repo doctor`. The tool executor also needs Bubblewrap, Python 3, Bash, and `prlimit` installed under `/usr`.

`doctor` marks the selected agents and their roles. An unavailable selected agent fails the check; `OPTIONAL` identifies unused harnesses that still need setup or qualification. Binary installation is listed separately from execution support. Adapters check minimum versions, supported interfaces, and actual filesystem/network restrictions. Newer stable versions can pass these checks without an exact-version allowlist. See the [harness guides](harness/README.md) for per-harness setup and the [qualification guide](native-qualification.md) for tested versions.

| Execution mode | Use it for |
| :--- | :--- |
| **`isolated` (default)** | The selected harness's verified tool boundary, with task files separated from live Git state and host credentials. Set `execution.backend: oci` for the optional container backend. |
| **`trusted-local`** | Repositories and tools you trust to run under your OS account. Select it explicitly with `SWARM_EXECUTION_MODE=trusted-local` where project policy permits. |

> [!NOTE]
> Model connectivity stays in the authenticated harness. Isolated tools can use the task snapshot, private scratch space, and system runtime files; tool network access is disabled. Install build tools and dependencies locally. The [capability table](ARCHITECTURE.md#tool-capabilities) describes editing, shell access, and extension restrictions. Native macOS and Windows execution still need qualification; Windows users can use a qualified Linux setup in WSL2. Authenticated OCI workers are a separate roadmap item.

Run `swarm repo doctor` and `swarm issue work 42 --dry-run` to check the setup before claiming work. See [project configuration](project.md#shared-defaults-and-personal-choices) for shared defaults and agent presets, and [execution modes](../SECURITY.md#execution-modes) for runtime setup. `--backend local` selects local coordination storage; execution mode still controls how workers run.

---

## Agent integration runbook

Every usable coding tool is configured under `agents:` in `.swarm/config.yaml`. The configuration key is the policy and audit name, while `harness` selects one of SwarmOS's four built-in CLI drivers (`claude`, `codex`, `opencode`, or `omp`; defaults to the key name when omitted).

The following are the `trusted-local` CLI drivers. Isolated adapters use the same bounded tools for both working styles; pairing streams the response and accepts follow-up instructions.

| Harness Driver | Headless Invocation | Interactive Invocation |
| :--- | :--- | :--- |
| **[Claude Code](https://code.claude.com/docs/en/setup)** (`claude`, checked with `2.1.308`) | `claude -p --append-system-prompt-file <prompt> <task>` | `claude --append-system-prompt-file <prompt> <task>` |
| **[OhMyPi](https://github.com/can1357/oh-my-pi#install)** (`omp`, checked with `18.12.5`) | `omp -p --no-pty --append-system-prompt <prompt> <task>` | `omp --append-system-prompt <prompt> <task>` |
| **[OpenAI Codex](https://learn.chatgpt.com/docs/codex/cli)** (`codex`, native integration checked with `0.163.0`) | `codex exec <combined prompt>` | `codex <task>` |
| **[OpenCode 2](https://dev.opencode.ai/v2/docs/)** (`opencode`, V2; checked with `2.1.2`) | `opencode run --standalone <combined prompt>` | `opencode mini --standalone --prompt <task>` |

When an entry sets `model: <name>`, SwarmOS passes that exact choice to the harness. With no model set, the harness selects its default; SwarmOS has no built-in model-name mapping. Isolated runs use controlled execution settings, so choose a project model here when you need a consistent selection across workers. For offline tests and local dry runs (`--backend local`), you can also configure the deterministic built-in `mock` runner with explicit `trusted-local` execution.

### Grok Bot and OpenAI Dot

Want to work through an always-on conversational teammate in addition to local CLI drivers?
- **[Working with Grok Bot & SwarmOS](grokbot.md)** — Connect Grok Bot to GitHub for repository conversations and PR reviews, run `swarm` on Grok Bot's cloud computer, or configure an xAI Grok model via `opencode`.
- **[Working with OpenAI Dot, Codex & SwarmOS](openai-dot.md)** — Add the repositories you want to swarm on to a Codex Cloud environment or connected dev machine and let your Dot coordinate approved tasks.

---

## Reviewer isolation and credentials

During an automated agent review (`--gate agent`), SwarmOS runs the reviewer against a separate read-only snapshot of the worktree where the only imported output is `.swarm/active/REVIEW.md`. In `isolated` mode, the selected native or OCI backend enforces this boundary. In `trusted-local` mode, choose a reviewer sandbox:

- **macOS**: Uses `sandbox-exec` to block writes outside the temporary scratch space and review output channel.
- **Linux**: Uses `bwrap` (`bubblewrap`) with user, IPC, PID, UTS, and default network namespace isolation. On AppArmor-enabled Linux distributions, ensure the distribution's `bwrap` AppArmor profile is installed (Ubuntu 26.04 includes this out of the box; on Ubuntu 24.04 see [LP #2069526](https://bugs.launchpad.net/ubuntu/+source/apparmor/+bug/2069526) or use an OCI container runtime).
- **OCI Container (`sandbox: container`)**: Supported on Linux, macOS, and Windows (where no native host sandbox exists) using `docker` or `podman` with a read-only root filesystem, dropped capabilities, and explicit scratch/output mounts.
- **Reviewer network policy**: Reviewer network access is disabled by default; set `review_network: true` on a reviewer agent entry when it needs outbound API access to reach its model provider.

In `trusted-local` mode, agent subprocesses start with a clean temporary `HOME` and receive only the model-authentication environment variables explicitly listed in `agents.<name>.env` (publishing/SSH variables such as `GH_*`, `GITHUB_*`, `GIT_*`, and `SSH_*` remain with the `swarm` coordinator process). Configure credentials through those allowed variables; your usual home-directory login and settings are not automatically loaded. Planner, developer, and tester roles execute your project's build and test tools inside their assigned worktree as host processes under your user account. Isolated workers receive no host credential variables.

---

## 2. Checkpoints, Handoffs & Recovery

Fresh same-repository claims on the GitHub and local backends start at the current default-branch tip when the WorkUnit and committed `.swarm/` tree are unchanged and the old base is an ancestor. The command prints both bases. With no plan artifacts, planning and review run at the new base; existing plan artifacts require a complete authenticated review that can carry forward. Policy, role prompts and validation then load from that new immutable base.

A human gate, changed task or policy, incomplete review evidence, or failed ancestry check requires `swarm issue approve <issue>`. Direct claims stop with the reason; queued work reports the issue and tries the next candidate. Existing checkpoints, branches, PRs and fork claims keep their pinned base. Human-gated claims remain available when the base has not moved.

Every claim records a unique fencing token (`claim_id`), approved base SHA, branch name, and checkpoint commit SHA. The local claim token is stored in your user config directory (`swarm/claims/<git-common-dir-hash>/<issue>.json`) so same-machine commands resume cleanly without colliding.

### Readable task history

Task commits use the approved issue title and number instead of a generic checkpoint message. Automated retries after failed validation identify repair commits; submission identifies any final changes it needs to commit. A clean worktree does not produce another commit. Maintainers can choose subject-only messages or include the objective and staged-file summary with [`commit_message.detail`](project.md#checkpoint-commit-messages).

SwarmOS opens Draft PRs without squashing or rewriting task history. GitHub's **Squash and merge** combines the PR commits into one commit on the base branch; **Create a merge commit** and **Rebase and merge** retain individual task commits. Under **Settings → General → Pull Requests → Allow squash merging**, choose the default squash message format. For a compact final history, use the PR title and review the message before merging; including the PR description also includes its longer artifact and review context.

### Handing Off a Plan or Review to Another Teammate
One teammate or node can complete the planning phase, checkpoint the branch and artifacts to Git, and hand the task off for another worker to review or implement:

```bash
# Teammate A generates and checkpoints PLAN.md + TEST_PLAN.md, then releases into handoff mode
swarm issue work 42 --phase plan --handoff --backend github

# Teammate B (or a worker node) picks up the handed-off task and continues from the verified checkpoint
swarm issue work --next --all --backend github
```

### Taking a Submitted PR Through Acceptance Review

`swarm issue submit <issue>` hands maintainers a linked Draft PR with the plan, test plan, implementation summary, and validation results together. The approved issue remains the work contract; the PR proposes its implementation. `swarm issue status <issue>` shows the recorded PR number and URL. A `submitted` phase and `completed` lease mean the worker has finished its submission, not that the PR has been reviewed or merged.

Before publishing, `swarm issue submit` checks the selected target branch against the approved base. If it has moved, the command reports how many target commits the base is behind and checks whether the validated checkpoint merges cleanly. Conflicts are informational: submission still opens the draft PR, lists the conflicted paths, and adds the recovery command to its description. For PR #123, run `swarm pr rebase 123 --resolve`; fork contributors use `swarm --fork pr rebase 123 --resolve` (add `--fork-remote <name>` for a custom fork remote).

The moved base needs approval re-pinned before or during rebase. Fork contributors first need a maintainer to run `swarm issue approve <issue>` upstream; see the approval steps below. Submission never rebases the branch or changes its approval. If the target cannot be fetched or inspected, it reports a skipped freshness check and proceeds. Results describe the target observed at submit time; it can move again afterward. `--json` includes a `base_freshness` object with status, drift, behind count, and conflicted paths; unknown drift and counts are `null`.

The PR's acceptance checkboxes are a **maintainer review checklist**. SwarmOS copies the requirements from the issue and leaves them unchecked; it does not yet store a verification decision for each criterion. `swarm issue approve <issue>` first authorizes execution of the WorkUnit; `swarm issue review-plan <issue> --approve` separately authorizes the implementation plan before code changes. Neither approves the finished code. Passing required checks provides evidence for GitHub PR review.

1. Open the PR and use its attached test plan to find the verification for each criterion.
2. Compare the implementation with the observed validation results. Run any remaining targeted tests or manual scenarios, record the evidence in the PR, and check off verified criteria in the PR description. Leave anything unverified unchecked.
3. Mark the Draft PR **Ready for review** in GitHub, or use `gh pr ready <PR-number>`. If it is already ready, request a code review directly.
4. Complete the repository's normal code review and CI requirements, then merge. `Closes #<issue>` closes the linked issue when the PR merges into the default branch; it does not synchronize the issue's and PR's checkboxes.

Use GitHub for final code and acceptance review. Retrying an already completed `swarm issue submit <issue>` returns the existing PR rather than refreshing its description. To change that PR, use its PR number with the commands below; issue commands continue to take issue numbers.

### Continuing an Existing PR

Use the same verbs for the same kind of work: `issue work` implements an approved issue, while `pr work` continues its proposed implementation. `issue stage` and `pr stage` both record interactive plans or implementation checkpoints, but PR staging reads only the separate PR worktree and leaves the original issue branch unchanged. Fixing a failing test or addressing feedback is development, not a separate `repair` operation. `pr rebase` changes branch ancestry and does not ask an agent to rewrite the implementation.

PR commands require an open PR recorded in an approved WorkUnit's authoritative state. A closing reference in a PR description helps SwarmOS find that link quickly, but the link itself always comes from coordination state.

Before claiming work or starting agents, `pr work` checks GitHub mergeability and compares the target branch tip with the issue's approved baseline. Known conflicts stop early with commands for the linked PR and issue. If GitHub is still computing mergeability, SwarmOS retries briefly (up to three observations within three seconds), then continues with an unknown notice and the usual approval checks. `pr work --dry-run` reports these results, blockers, and recovery commands as JSON without taking a claim. The local backend skips this GitHub check.

Who can revise a PR:

- **Maintainers** with upstream write permission can revise any linked PR. For a PR whose head lives in a contributor's fork, add a writable Git remote for that fork and select it with `--fork-remote <name>` (default `fork`), without `--fork`.
- **Contributors** revise their own fork PRs with `swarm --fork pr ...`. Changes publish to the branch in their fork, and the upstream fork coordinator records each step after checking that they own that fork, that the published head is the one they validated, and that the revised plan is approved. See [Revising your PR](fork-mode.md#revising-your-pr-after-review).

`pr status` shows the published head and target, local revision, check runs, and GitHub review records with their commit SHAs. Checks on the published head are separate from local `pr validate` results; reviews against an older commit remain visible as history. When the tracker reports no CI checks (for example, on the local backend), `pr status` shows the SwarmOS validation recorded for the published head as a `swarm-validation` check.

When checks fail or are cancelled on the published PR head, `swarm pr work <PR>` automatically prepares `.swarm/active/CI.md` in the PR worktree. It includes each affected check's name, conclusion, tested SHA, annotations, and the tail of its GitHub Actions job log. The developer receives a pointer to this quoted diagnostic evidence and works within the approved plan. You can still use `--instructions` to describe the outcome you want.

Logs retain the newest 32 KiB per check, with 128 KiB of logs and 64 KiB of annotations across a report; the complete file is capped at 256 KiB. Truncation is marked, terminal control characters are normalized, and GitHub's `***` masking is preserved. Expired logs, missing Actions read permissions, cancelled jobs without logs, and other check providers keep their available metadata with a note explaining that the log is unavailable. The coordinator retrieves this evidence with its own credentials. Checks from older heads are excluded, and each work run clears previous evidence when there are no current failures. The local backend continues without CI data.

On the GitHub backend, `pr status` also lists open code-scanning alerts (for example, from CodeQL) on the PR's merge ref, `refs/pull/<PR>/merge`. Each alert shows its rule ID, severity, `path:line`, and message, and `--json` includes them under `result.code_scanning`. When checks fail, `pr work` reads the same alerts once and adds them to `CI.md` ahead of logs and annotations, so the developer sees the finding behind a failed `CodeQL` check. Alerts are read from the upstream repository in both same-repository and `--fork` mode. Alert text is untrusted: it is escaped for the terminal and quoted in `CI.md`. At most 50 alerts are kept, each field is size-limited, and the alert section of `CI.md` is capped at 32 KiB. Alerts that don't fit are counted (as `at least` a number when SwarmOS stopped reading after 1,000), and truncated text is marked. If the token lacks code-scanning read access (`security_events`), code scanning is not enabled, or the API fails, both commands print `unavailable (no access or not enabled)` and continue. The local backend reports no alerts. Check metadata now reserves 512 bytes of the 256 KiB report for these notices.

If commits are pushed to the PR branch outside SwarmOS, `pr status` reports `unverified-head`: the recorded plan, validation, and provenance attestation describe the last verified head, not the current one. Run `swarm pr work <PR>` to start a revision from the current head; it re-plans, re-validates, and records new evidence before `pr submit`.

```bash
# 33 is the PR number; its linked WorkUnit may have a different issue number.
swarm pr status 33 --backend github
swarm pr work 33 --dry-run --backend github
swarm pr work 33 --instructions "Fix the failing checks within the approved scope" --backend github
swarm pr validate 33 --backend github
swarm pr submit 33 --backend github
```

When an approved plan is available, PR work writes a focused revision plan: each change names the finding or request it addresses, the affected acceptance criterion, and how to verify it. The planner receives the original plan and test plan, accepted revisions, current CI evidence, and GitHub review summaries and inline comments. Review evidence is quoted and bounded; it supplies diagnostic context while the WorkUnit defines scope.

The original pair is available as `.swarm/active/ORIGINAL_PLAN.md` and `ORIGINAL_TEST_PLAN.md`, with accepted deltas in `ACCEPTED_REVISIONS.md` and review evidence in `PR_REVIEW_EVIDENCE.md`. Write the active delta to the usual `PLAN.md` and `TEST_PLAN.md`. The configured review gate still applies to that exact pair. Issue status, PR status, and the managed PR description distinguish the original from the current revision after submission too. Without an eligible original pair, planning uses the full-plan workflow.

Changes are prepared in `.swarm/worktrees/pr-33`, separate from the original issue worktree. A new revision goes through planning and the configured review gate again. For a human gate, an independent maintainer approves the linked **issue's** plan with `swarm issue review-plan <issue> --approve`, then the worker resumes `swarm pr work <PR>`. Use the existing `--agent`, role-agent overrides, `--phase`, `--headless`, `--progress`, and `--json` flags as appropriate.

For `plan_mode: interactive`, `swarm pr work <PR>` pauses at `awaiting-plan` after preparing `.swarm/worktrees/pr-<PR>`. Write `.swarm/active/PLAN.md` and `.swarm/active/TEST_PLAN.md` **in that PR worktree**, then run `swarm pr stage <PR> --phase plan --harness <name> --model <name>` (optionally `--handoff` or `--json`). Under an agent gate, resume with `swarm pr work <PR>` to review and implement; under a human gate, an independent maintainer uses `swarm issue review-plan <issue> --approve` before `swarm pr work <PR>` resumes implementation. Do not stage through `swarm issue stage` against the old issue worktree while the PR revision is pending.

Alternatively, after the plan gate, make implementation changes in the managed PR worktree and run `swarm pr stage <PR> --phase implement --harness <name> --model <name>`. This records a local checkpoint and validates against the approved baseline; it does **not** publish the branch or change the submitted issue checkpoint. Inspect with `swarm pr status <PR>` and publish only with `swarm pr submit <PR>`. Staging requires the existing pending revision, a valid active issue claim, and upstream mutation authorization (or, with `--fork`, a claim on your own fork PR); it rejects rebases, diverged heads, and unrelated worktrees.

Run every required check before you finish, not only build and test, and fix lint and format findings locally rather than leaving them for CI. The shipped developer prompt asks agents to do the same. Host-executed checks reuse a persistent SwarmOS-owned Go build cache for the repository, so repeat runs are faster than the first.

Explicit `swarm issue validate` and `swarm pr validate` always execute checks. Submit can reuse a passing run recorded by the coordinator for the same worker and claim, using a released swarm build. The candidate checkpoint tree, approved base, trusted check policy, and swarm version must all match. Issue validation can cover uncommitted changes: its fingerprint uses the same content that the next checkpoint commits, without changing your index or files. PR validation requires a clean recorded revision.

On reuse, stderr names the original run, its age, and a short tree SHA; the validation artifact and PR description carry the same provenance. JSON reports set `reused: true`. Any mismatch, missing evidence, failed run, development or dirty build, or fork submission runs checks again. CI always runs independently.

Failed checks in `swarm repo validate`, `swarm issue validate`, `swarm pr validate`, and issue/PR submission show the check name, exit code, and output tail before the command error. The tail retains the end of the output, up to 60 lines and 16 KiB including a truncation marker, with terminal controls escaped. Go test failures also produce a bounded `failing tests:` list, including names found before the retained tail. These details remain available with `--progress=false`.

With `--json`, failed check outcomes add `output_tail` and `failing_tests` (an empty array for non-Go failures); passing outcomes omit both fields. Issue submission failures include the validation `report` alongside `kind`, `status`, and `error`. Existing raw output and validation artifact fields remain available.

Issue and PR `validate` and `submit` commands report phases, check starts, and PASS/FAIL results with elapsed time on stderr. A running line appears every 15 seconds during a check or phase. Add `--progress=false` for quiet operation; stdout, PR URLs, and `--json` results stay the same. Issue submission creates a checkpoint and creates or updates its draft PR; PR submission publishes the existing checkpoint and updates review evidence and rebase metadata.

`pr submit` runs trusted validation or reuses matching passing evidence from the current claim and publishes to the existing PR branch. It preserves the PR's title, human-written description, draft/ready state, and identity, and updates a managed rebase context section; GitHub applies the repository's review rules to the new commits. Publication uses the remote head observed when the revision began; a concurrent remote update is rejected rather than overwritten. Same-repository publication updates the PR branch and issue coordination state atomically.

If the implementation step fails before it commits anything, the revision keeps its approved plan and review. `pr submit` then refuses with nothing new to publish, without claiming the issue or changing coordination state. Resume implementation with `swarm pr work <PR> --phase implement` (contributors use `swarm --fork pr work <PR> --phase implement`), then submit again.

#### Rebasing onto an Updated Base

The target is the existing PR's base branch, pinned to a full commit. When that branch moves ahead of the WorkUnit's approved baseline, `--resolve` can re-pin approval automatically for an actor with task-approval permission when the WorkUnit and committed `.swarm/` policy tree are unchanged. It records the actor and both bases in an issue audit comment. Changed tasks or policy require explicit `swarm issue approve`; fork workers also use that explicit approval step. Approval can't change while a claim is active, including your own claim from earlier `issue work`, so release it first. The next PR command claims the issue again automatically.

```bash
# 33 is the PR number, 16 its linked issue.
swarm issue release 16 --reason "re-approve against the new base for PR #33" --backend github
swarm issue approve 16 --backend github
swarm pr rebase 33 --backend github

# If Git reports conflicts, resolve them in .swarm/worktrees/pr-33 and stage them:
git -C .swarm/worktrees/pr-33 add path/to/resolved_file.go
swarm pr rebase 33 --continue --backend github
# Or abandon the in-progress rebase and restore the starting commit:
swarm pr rebase 33 --abort --backend github
```

A conflict at the current approved baseline needs a rebase, without another issue approval. When the target has moved, follow the release → maintainer approval → rebase sequence above. Contributors add `--fork --fork-remote <name>` to their PR commands; issue approval is a maintainer action against upstream, without fork flags. After a paused rebase, finish with `--continue` after staging files, use `--continue --resolve`, or restore the starting commit with `--abort`.

##### Let the developer agent resolve conflicts

Most rebase conflicts are routine: imports added next to each other, tests both branches appended, or a docs paragraph both sides reworded. Add `--resolve` and your configured developer agent handles them while SwarmOS keeps control of Git:

```bash
# Start the rebase and resolve each conflicted commit with the developer agent
swarm pr rebase 33 --resolve --backend github

# Already paused? Resolve what is still unmerged; files you staged by hand stay as they are
swarm pr rebase 33 --continue --resolve --backend github
```

At each conflicted commit, the agent first writes `.swarm/active/REBASE_PLAN.md` on a disposable copy of the conflicted files, both sides, the WorkUnit, and the approved plan. For each path it describes both sides, the intended resolution, and how the approved behavior is preserved. SwarmOS records this evidence before starting a separate resolution turn, and passes the accumulated micro-plan to later turns. The turn uses the developer role's normal time limits. SwarmOS accepts the result only when the agent changed nothing but those files and left no conflict markers. It then stages them and continues to the next commit. If the agent fails, times out, touches anything else, or leaves markers, the rebase stays paused exactly as Git left it, and the command lists the conflicted files with the manual `--continue` and `--abort` steps.

`--resolve` uses the same developer agent selection as `pr work`, so `--agent`, `--developer-agent`, `--headless=false`, and `--progress=false` work here too. Deleted or renamed files, links, binary or very large files, SwarmOS control paths, and protected paths (unless the WorkUnit sets `allow_harness_changes`) always stay paused for you. So do files that legitimately contain conflict-marker lines, such as marker test fixtures. `pr status` and the PR's rebase context list each agent resolution with its commit and provenance. After the rebase, `--resolve` runs checks from the approved base. If checks fail, the developer receives the validation report and micro-plan in a Git-free repair snapshot. Repair edits are limited to the PR's diff against the new base and the conflicted paths, with protected paths still enforced. Each accepted attempt becomes a local checkpoint and is validated again. The trusted configuration's optional `lease.rebase_repair_limit` sets the attempt limit: omission gives two attempts, and zero disables repair. Retrying a completed rebase retains its repair count.

On success, the command reports validation PASS and prints `swarm pr submit 33` (or `swarm --fork pr submit 33` for a fork). Submission remains a separate command. If repair fails, reaches its limit, or edits outside the allowed paths, SwarmOS keeps the last local checkpoint and prints the validation evidence and manual `pr work` / `pr validate` steps. Micro-plans and resolution reports appear in `pr status` and, after explicit submit, in the PR update; they never grant plan approval.

Use `--continue` or `--abort` only when a rebase is in progress. An approved plan carries forward when the WorkUnit and the committed `.swarm/` policy tree are unchanged, the previous baseline is an ancestor of the new one, and the review recorded that policy tree. In that case, run `swarm pr validate 33` and `swarm pr submit 33` right after rebasing. Implementation and validation evidence always need renewal, and a human gate still requires an authenticated human review.

After explicit task reapproval for a policy change, `--resolve` can ask the configured agent reviewer to review the unchanged, previously approved plan under the new policy. A human gate stops with `swarm issue review-plan <issue> --approve`; fork workers use maintainer review through the same command. Other stale or edited plans require a new plan cycle through `swarm pr work <PR>`. For interactive planning, write the replacement plan and test plan in the PR worktree and run `swarm pr stage <PR> --phase plan`, then resume `swarm pr work <PR>` through the configured plan gate. `pr status` explains why a plan could not carry forward and lists changed policy paths when available. Reviews recorded before policy tree binding need a fresh review after a baseline move.

A completed unpublished rebase can enter this new planning cycle even while GitHub reports the old published head as conflicting. SwarmOS checks the existing local worktree and recorded rebase evidence without changing them, and requires the rebase base, approved baseline, and current target tip to match. It prints `published head superseded by local rebase` when this applies; the new plan still follows its configured approval gate.

`pr status` and the submitted PR description show the old and new heads and bases, the paths that had conflicts, and the aggregate Git patch identity. Unchanged patch identity helps reviewers focus on conflict resolutions; trusted validation is still required either way. In fork mode, conflict paths are labeled as contributor reports, and the coordinator checks patch identity from the published commits. While a PR revision is pending, use the PR commands against its separate worktree rather than issue work/stage/validate/submit against the original issue worktree.

SwarmOS also compares the paths the PR changed against its old base with the paths it changes against the new base, after the rebase and after every accepted repair attempt. Each path the PR no longer changes is listed as `Warning: no longer changed: <path>` in `pr rebase` and `pr status` output. This catches a conflict resolution, union merge, or repair that silently threw away part of the PR, and also paths whose change already landed upstream. The comparison works on whole paths, not hunks. A path that is still changed is not reported, even if its contents differ, and paths newly changed by the rebase are never reported. The warning doesn't block validation or submission. Inspect each path with the `git diff <old-head> <new-head> -- <path>` command it prints. If the change was lost, restore it with `swarm pr work 33`. `swarm pr rebase 33 --abort` only applies while a rebase is paused; it can't undo a completed rebase. In `--json` output and the issue's rebase evidence, `last_rebase.dropped_paths` holds the paths and `last_rebase.dropped_paths_verified` is `true` once the comparison ran. Without that field, the comparison is unknown, and `pr status` prints `Changed-path comparison: unavailable`. In fork mode, the upstream comparison waits for the published commits, like patch identity. Until then, the contributor's own coordinator compares its local commits and reports them under `result.local_dropped_paths`, labeled as local and never recorded upstream.

> [!TIP]
> If a rebase step stops with one of these errors, the fix is usually one command:
>
> | Message | What to do |
> | --- | --- |
> | `rebase repair limit reached` | Keep the recorded local checkpoint, run `swarm pr work <PR>` for manual repair, then `swarm pr validate <PR>` and submit explicitly. |
> | `human plan approval required` | Ask an independent maintainer to run `swarm issue review-plan <issue> --approve`, then retry. |
> | `PR base tip differs from approved issue baseline` | Run `swarm issue release <issue>` if you hold a claim, then `swarm issue approve <issue>`, then retry the rebase. |
> | `issue is already claimed by another active lease` (from `issue approve`) | The message names the claim's owner and expiry. If it's yours, run the `swarm issue release <issue>` command it prints; otherwise wait for the claim to expire or ask its holder to release it. |
> | `rebase PR head … (conflicts preserved)` | Run `swarm pr rebase <PR> --continue --resolve` to let the developer agent try, or resolve the conflicts in `.swarm/worktrees/pr-<PR>`, `git add` each resolved file, then run `swarm pr rebase <PR> --continue`. |
> | `PR #<PR> has a paused rebase` (from `pr work` or `pr stage`) | Finish the rebase first: `swarm pr rebase <PR> --continue` after staging, `--resolve`, or `--abort`. |
> | `git rebase --continue: … needs merge` | Some files still have conflict markers or aren't staged. Check `git -C .swarm/worktrees/pr-<PR> status`, finish resolving, `git add` them, and continue again. |
> | `rebase already in progress` | Finish the paused rebase with `--continue`, or use `--abort`; don't start a new one. |
> | `PR worktree must be clean before starting rebase` | The PR worktree has uncommitted changes. Check `git -C .swarm/worktrees/pr-<PR> status`, keep them with `swarm pr stage <PR> --phase implement` or remove them, then retry. |

You don't need a separate command to resume a PR after its claim is lost or expires. `pr work`, `pr rebase`, `pr validate`, and `pr submit` each check the issue's lease first. Each one resumes your active claim when this machine holds its token, or takes a fresh claim when the old one has lapsed. `swarm pr status <PR>` shows where the revision stands.

### Reading a failed agent's output

SwarmOS never prints harness output in failure errors or `--json` results, because a harness can emit terminal escapes, prompts, or credentials. When `swarm issue work`, `swarm pr work`, or `swarm pr rebase --resolve` reports a failed agent, the error ends with a pointer to a private local copy instead:

```text
planner agent failed (exit 1; harness diagnostics withheld); diagnostics saved to /home/you/.local/state/swarm/diagnostics/failure-3f9c….log (2048 bytes)
```

Open that file with any pager, for example `less <path>`. It holds the last stdout and stderr output of the failed run:

- Location: `$XDG_STATE_HOME/swarm/diagnostics/` when `XDG_STATE_HOME` is set, otherwise `~/.local/state/swarm/diagnostics/` on Linux and the user configuration directory (for example `~/Library/Application Support/swarm/diagnostics/`) on macOS and Windows. It is outside every worktree and is never committed, uploaded, or shown on issues or PRs.
- Permissions: the directory is `0700` and each file `0600`. SwarmOS refuses to write through a symlink or into a directory other users can read.
- Contents: at most 64 KiB of the most recent output. The coordinator's GitHub token is redacted the same way as in Git diagnostics. Control characters, terminal escapes, and invalid UTF-8 are written as printable escapes such as `\x1b`, so viewing the file cannot drive your terminal. Other secrets a harness prints are not detected, so treat the file as private.
- Retention: logs older than seven days are deleted, and only the newest 20 are kept.

When no private copy can be written, the error ends with `diagnostics unavailable: <reason>` instead of a path. SwarmOS never falls back to another location. The reason is always one of these:

- `state directory unavailable`: SwarmOS could not determine your user state directory, for example because no home directory is set. Set `XDG_STATE_HOME` to an absolute path.
- `unsafe state directory`: the state directory is relative, contains control characters, passes through a regular file, or resolves inside the agent's worktree.
- `private directory unavailable`: the `swarm/diagnostics` directory could not be created or opened without following symlinks, or other users can access it. Remove the symlink or run `chmod 700` on the directory.
- `credential redaction unavailable`: the coordinator's GitHub token could not be resolved for redaction, so nothing was written. Check `SWARM_GITHUB_TOKEN` or `SWARM_GITHUB_TOKEN_COMMAND` (see [GITHUB.md](GITHUB.md)).
- `log write failed`: the log file could not be locked or written, for example because a symlink or other non-regular file already sits at its path.

A run that printed nothing reports `no harness output to save`. Successful runs save no log. Isolated workers are not given access to the diagnostics directory. Trusted-local agents run as your user, though, so they can read earlier logs. The files are private from other users, not from trusted-local agents. Interactive (`--headless=false`) runs still stream harness output live to your terminal.

### Recovering from an Interrupted Run

When native Codex or a harness-tools Bubblewrap run reaches its agent budget, SwarmOS leaves the task worktree and role artifacts untouched and reports a recovery patch at `.swarm/active-output/<role>-timeout.patch` in the original task workspace, including for reviewers. Inspect that patch before recovering useful edits manually in a separate checkout. It compares the sandbox's starting files, including dirty and untracked inputs, with its final contents; those starting files may differ from HEAD.

Recovery patches are limited to 4 MiB. Binary files, oversized files, and entries that would exceed export bounds are omitted with a diagnostic. Unsafe symlinks or special files stop preservation and produce an explicit failure message. An empty patch means no recoverable text changes were exported. A later timeout for the same role replaces the previous patch, so copy one you want to keep.

Recovery output is excluded from snapshots and checkpoints. Submission rejects recovery files that were manually staged or committed. Patches are never applied or registered as artifacts automatically. OCI timeouts report that no patch is available.

Agent budgets default to 60 minutes. Interactive native Codex and Bubblewrap sessions exclude waits at coordinator input prompts and have a four-hour wall cap. Interactive OCI counts all wall time, capped at four hours and shortened by an explicit approved timeout. Configure global and per-role budgets in [Agent run budgets](project.md#agent-run-budgets); a warning appears shortly before the deadline.

If an agent exits early, a lease expires, or a validation check fails:
1. Run `swarm issue status <issue> --backend github` to inspect the current lease, phase, and published artifacts.
2. Fix the code, test, or agent configuration inside `.swarm/worktrees/issue-<N>`.
3. Re-run `swarm issue work <issue> --backend github`—if your claim token is still active, SwarmOS resumes in place; if the lease expired, it acquires a fresh attempt on the verified checkpoint branch.
4. Run `swarm issue validate <issue>` and `swarm issue submit <issue>` when all checks pass.

An implementation that produces no checkpoint beyond the approved base fails before testing. An explicit blocked outcome in `IMPLEMENTATION.md` also fails, and `swarm issue status <issue>` shows its reason. For PR revisions, the implementation must advance the revision's starting head. Issue reruns can reuse an existing checkpoint ahead of the approved base to repeat testing.

Steer a recovery with specific instructions:

```bash
swarm issue work 56 --phase implement --instructions "Use temporary Git repositories for the Git-dependent tests"
swarm pr work 123 --phase implement --instructions "Fix the failing acceptance test"
```

Flag instructions and supported interactive follow-ups appear in role reports and PR review evidence, with a message count and SHA-256 in status output. Earlier same-role reports are marked as superseded attempts. Lessons from the current task stay in review evidence until the PR merges and a maintainer applies them with `swarm repo lessons --apply`.

After a plan review requests changes, the default `swarm issue work <issue>` or `swarm pr work <PR>` runs the planner again with the review findings. The planner must change `PLAN.md` or `TEST_PLAN.md` before another review; changing only operator guidance does not qualify.

`swarm issue submit` refuses an empty branch and points to an implementation rerun. Claims saved by this version carry a local base hint so this refusal happens before contacting GitHub. Older claim files use the authoritative check after loading state; running `swarm issue work` refreshes the hint. Real uncommitted edits are checkpointed normally.

If a checkpoint no longer descends from the approved base, a maintainer can re-approve the issue with `swarm issue approve <issue> --backend github`. Release any active claim first with `swarm issue release <issue> --backend github`. When the new base contains every commit in the checkpoint, approval drops that checkpoint and reports it. The next `swarm issue work` moves the clean local task branch and worktree forward to the new base; the remote task branch advances with the next checkpoint publication. Eligible plan approval carries forward when the WorkUnit and policy are unchanged, while implementation and validation evidence are cleared.

Save or move aside local edits before resuming. A checkpoint containing commits absent from the new base still requires explicit reconciliation; SwarmOS keeps those commits and refuses to reset the branch. A task branch changed by someone else also requires inspection before recovery can continue.

A task can also be re-approved on a newer base before it has any checkpoint, for example after planning paused for human review. The next `swarm issue work <issue>` (or `swarm issue claim`) fast-forwards the local task branch and worktree to the new base, provided all of these hold:

- the branch and worktree are still exactly at the previously approved base;
- the new base descends from it;
- the worktree has no changes other than SwarmOS's own `.swarm/active/` files and the generated pointer in `AGENTS.md` and `CLAUDE.md`;
- checking out the new base would not overwrite any untracked or ignored file.

The branch moves with a compare-and-swap, so if anything else moved it, nothing changes. SwarmOS then rebuilds `.swarm/active/` from the issue's recorded state. The carried plan, test plan and review come from that state, never from files left in the old worktree. This applies to same-repository work only; `--fork` tasks keep the existing behavior.

When a worktree has commits or edits of its own, SwarmOS still refuses and prints the recovery commands. First copy out any edits you want to keep. Then remove the worktree with `git worktree remove --force -- '<path>'` and rename the task branch with `git branch -m '<branch>' 'swarm-backup/issue-<N>'`, which keeps its commits. Finally, run `swarm issue work <issue>` again.


---

## 3. Multi-Repository Registry & Worker Pools

Register the repositories you want to work on so `swarm issue claim --next --all`, `swarm issue work --next --all`, and `swarm node run --all` can coordinate across them:

```bash
# Register the current checkout or clone a remote GitHub repository
swarm repo add --backend github
swarm repo add OWNER/REPO --clone --backend github
swarm repo list

# Initialize a worker node profile (~/.config/swarm/node.yaml)
swarm node init

# Drain up to 4 ready tasks across registered repositories with 2 concurrent workers
swarm node run --all --concurrency 2 --max-jobs 4 --auto-submit --backend github
```

Workers use the same selection order as `swarm issue claim --next` and `swarm issue work --next`, skipping candidates that are not eligible. By default, `swarm node run` drains the currently available ready queue and exits cleanly. Add `--watch` (and optional `--poll-interval 30s`) when you want the worker pool to keep polling for newly approved tasks.

---

## Related Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](ADMIN_GUIDE.md)** — Roadmap decomposition (`swarm repo plan`), backlog triage, and Draft PR auditing.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](GITHUB.md)** — Branch rulesets, `SWARM_GITHUB_TOKEN`, and collaborator allowlists.
- **[Project & Maintainer Setup (`docs/project.md`)](project.md)** — Initialize `.swarm/`, configure agents and checks, and approve tasks.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS state refs, fencing, and checkpoint mechanics.
- **[Security Policy (`SECURITY.md`)](../SECURITY.md)** — Trust boundaries, reviewer isolation, and release verification.

### Revising rejected plans

After `CHANGES_REQUESTED`, issue and PR work print the review identity and the next planning command. Run that command once, then review with `swarm issue review-plan <issue>`. Work does not repeat planning and review within the same invocation. By default, the initial rejected plan can receive two additional automatic planner attempts; failed or unchanged attempts count toward that limit.

Check `swarm issue status <issue>` for the count and limit. When automation reaches the limit, stage revised reports in the appropriate issue or PR worktree (`swarm issue stage <issue> --phase plan` or `swarm pr stage <PR> --phase plan`), request an authenticated human review, or revise the WorkUnit for maintainer approval. Successful non-fork staging resets the count; fork staging preserves it and needs a maintainer review or changed approved revision to reset. `plan_mode: agent` disables manual staging, while `plan_mode: interactive` always pauses for it without spending automatic attempts. Node workers move past capped tasks. See [the recovery workflow](WORKFLOWS.md#recovering-from-a-rejected-agent-plan) for the complete sequence.

### Reacting to stops in scripts

`swarm issue work`, `swarm issue submit`, `swarm pr work`, `swarm pr rebase`, and `swarm pr submit` tell scripts and driving agents why they stopped, without any prose parsing. When one of these commands stops for a known reason, its `--json` failure document adds a `next_action` object, and the process exits with a code for that reason:

```json
{"kind": "work", "status": "failed", "error": "...", "recovery": "...",
 "next_action": {"reason": "changes_requested", "argv": ["swarm", "--backend", "github", "issue", "work", "42", "--phase", "plan"], "human_required": false}}
```

- `reason` is `changes_requested`, `needs_approval`, or `conflict`.
- `argv` is the suggested command as separate literal arguments, starting with `swarm`. Run it without a shell, or quote each element yourself. It repeats the `--backend`, `--repo`, and `--root` you used, and for worker commands `--fork` and `--fork-remote`. Maintainer commands (`issue approve`, `issue review-plan --approve`) are run upstream, so they leave out fork flags, and in fork mode they also leave out `--root`.
- `human_required` is `true` when a person has to act, such as a maintainer approving or someone resolving files by hand.

`next_action` is advisory. It never grants authority and contains no credentials: the command it names still goes through every approval, claim, and policy check. Existing fields such as `error`, `recovery`, `report`, and `result` are unchanged.

| Exit code | Meaning |
| --- | --- |
| `0` | Success. No `next_action`. |
| `1` | Any other failure, including invalid arguments, claim fences, unknown mergeability, and approval problems that don't name a single human decision. No `next_action`. |
| `3` | `changes_requested`: the agent reviewer returned `CHANGES_REQUESTED`. |
| `4` | `needs_approval`: a maintainer has to approve before work can continue. |
| `5` | `conflict`: the PR conflicts with its base, or a managed rebase is paused on conflicts. |

Code `2` is left unused so it stays free for usage errors. Text mode exits with the same codes, so `set -e` scripts and anything that checks for a nonzero status keep working.

Each reason suggests these commands:

| Reason | Suggested `argv` | `human_required` |
| --- | --- | --- |
| `changes_requested` | `swarm issue work <issue> --phase plan`, or `swarm pr work <PR> --phase plan` | `false` |
| `needs_approval` (task) | `swarm issue approve <issue>`. This is the linked issue, even when a PR command stopped. | `true` |
| `needs_approval` (human plan gate) | `swarm issue review-plan <issue> --approve` | `true` |
| `conflict` (GitHub reports the PR as conflicting) | `swarm pr rebase <PR> --resolve` | `false` |
| `conflict` (a managed rebase is paused) | `swarm pr rebase <PR> --continue --resolve` | `false` |
| `conflict` (the resolver left files for you) | `swarm pr rebase <PR> --continue`, after you resolve and stage the files | `true` |

A stale task approval comes before a conflict. If the PR's target base has moved, `pr work` reports `needs_approval`, even when GitHub also reports conflicts, because the new base needs approval before any rebase. If a managed rebase is already paused, finishing it comes first. If the reviewer rejected the plan, that rejection is reported over any other stop.

Not every command can return every reason:

| Command | `changes_requested` | `needs_approval` | `conflict` |
| --- | --- | --- | --- |
| `issue work` | yes | yes | no (there is no PR yet) |
| `issue submit` | no (it runs no review) | yes | no (conflicting submissions still open the draft PR) |
| `pr work` | yes | yes | yes (GitHub mergeability, or a paused rebase on any backend) |
| `pr rebase` | yes (re-review of a carried plan under `--resolve`) | yes | yes |
| `pr submit` | no (it runs no review) | yes | yes (a paused rebase) |

A human plan gate that pauses after planning is a successful run. `issue work` prints `"status": "paused"` and `pr work` sets `result.work.PausedForHuman`. Both exit `0` and have no `next_action`. Waiting for `swarm issue review-plan <issue> --approve` is the expected next step there. Dry runs (`--dry-run`) never report a `next_action`.

```bash
swarm issue work 42 --json > outcome.json
case $? in
  0) echo "done or paused at a human gate" ;;
  3|4|5) jq -r '.next_action.argv | @sh' outcome.json ;;  # inspect, then run the suggestion
  *) echo "failed: $(jq -r .error outcome.json)" ;;
esac
```

## Changelog fragments

Independent fragments let parallel contributions merge without sharing a changelog edit. Add a file under `changes/unreleased/` for each change and leave `CHANGELOG.md` for maintainer releases. Use a unique issue-based name such as `126.md`; add a descriptive suffix such as `126-fix.md` if another fragment uses that number.

Each file contains one `- ` Markdown bullet. Indented prose continuation lines are allowed; additional list items, nested lists, headings, and unrelated paragraphs are rejected. For example:

```markdown
- Add a release helper that combines changelog fragments.
```

Maintainers fold all fragments in filename order and delete the consumed files with:

```bash
make changelog VERSION=1.2.3
```

The command inserts a version heading below the empty Unreleased heading and preserves released history. With no fragments it changes nothing. All fragments are validated before the changelog is replaced. Replacement and fragment deletion are separate operations: if deletion fails, inspect the changelog and remaining files before retrying to avoid duplicate entries. Review the resulting changes before committing the release.
