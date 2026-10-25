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

Helpful CLI conventions:
- **Picking up the next task**: Use `swarm issue claim --next` to reserve the next eligible `swarm:ready` issue, or `swarm issue work --next` to claim and execute it immediately. Add `--all` with `--next` (or `swarm issue claim --next --all`) to scan across all repositories registered in `~/.config/swarm/repos.yaml`.
- **Interactive & harness-driven staging (`swarm issue stage`)**: Working inside an interactive coding harness without spawning nested subprocesses? Run `swarm issue claim <issue>`, write `.swarm/active/PLAN.md` and `TEST_PLAN.md`, and stage each phase directly with `swarm issue stage <issue> --phase plan --harness <name> --model <name>` and `swarm issue stage <issue> --phase implement --harness <name> --model <name>`.
- **Token auth without `gh` CLI**: Set `SWARM_GITHUB_TOKEN` (or `GH_TOKEN` / `GITHUB_TOKEN`) to authenticate directly with GitHub REST and Git HTTPS remotes on headless machines without installing `gh`.
- **Secret-manager tokens**: Set `SWARM_GITHUB_TOKEN_COMMAND` (for example `op read op://Private/GitHub/swarm-token` or `security find-generic-password -s swarm-github -w`) so the token stays in 1Password, Bitwarden, Keychain, or libsecret instead of your shell. See [Keep tokens out of your shell](GITHUB.md#keep-tokens-out-of-your-shell-with-a-secret-manager).
- **Headless vs. interactive**: `swarm issue work` runs non-interactively (`--headless=true`) by default. Pass `--headless=false` to follow the agent's work and add instructions between turns.
- **Structured JSON output**: Use `--json` on a leaf that supports it, such as `swarm issue claim --next --json`, `swarm issue work 42 --json`, `swarm issue status 42 --json`, or `swarm repo validate --base <trusted-full-commit-sha> --json`. Bare `swarm issue` and `swarm repo` are command groups, not JSON-producing operations.

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

Run `swarm issue cleanup 42 --apply --backend github` to retire an eligible upstream remote task branch. Cleanup requires the merged PR's final head to match the recorded submitted head, including for squash and rebase merges. An advanced branch, incomplete merge evidence, active attempt, pending revision, or branch used by an open PR blocks retirement. Fork branches remain with their owners.

Remote deletion and its state progress record use one atomic Git push with exact expected heads. Local branches and worktrees stay in place: Git's worktree removal can discard ignored files, and local checkout activity cannot be fenced by a remote state reservation. State and contributor ledgers remain available for status and provenance inspection.

If an interrupted cleanup leaves a reservation, run `swarm issue cleanup 42 --recover-reservation --backend github` to fence that operation and preview again, then rerun cleanup explicitly. Recovery retains the operation's audit history. GitHub cannot transact PR creation together with Git ref deletion; cleanup rechecks open PRs immediately before deletion, but a PR created after that check can race with it.

### Choose how you work

Start interactively when you want to stay close to the changes. Move repeatable tasks to unattended workers once the tools and permissions are set up.

| Working style | How to use it |
| :--- | :--- |
| **Pair with an agent** | Run `swarm issue work 42 --headless=false`. Native Codex streams its work, then accepts a follow-up instruction; press Enter to finish the phase. |
| **Stay in your current agent session** | Use `swarm issue claim <issue>` or `swarm issue claim --next`, work in the assigned worktree, and record each phase with `swarm issue stage`. Your existing session keeps its own permissions. |
| **Let a worker pick up tasks** | Run `swarm issue work --next` for one task or `swarm node run --watch` for a continuing queue. Both run headlessly; configure the harness to complete allowed work without waiting for keyboard input. |

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

The PR's acceptance checkboxes are a **maintainer review checklist**. SwarmOS copies the requirements from the issue and leaves them unchecked; it does not yet store a verification decision for each criterion. `swarm issue approve <issue>` first authorizes execution of the WorkUnit; `swarm issue review-plan <issue> --approve` separately authorizes the implementation plan before code changes. Neither approves the finished code. Passing required checks provides evidence for GitHub PR review.

1. Open the PR and use its attached test plan to find the verification for each criterion.
2. Compare the implementation with the observed validation results. Run any remaining targeted tests or manual scenarios, record the evidence in the PR, and check off verified criteria in the PR description. Leave anything unverified unchecked.
3. Mark the Draft PR **Ready for review** in GitHub, or use `gh pr ready <PR-number>`. If it is already ready, request a code review directly.
4. Complete the repository's normal code review and CI requirements, then merge. `Closes #<issue>` closes the linked issue when the PR merges into the default branch; it does not synchronize the issue's and PR's checkboxes.

Use GitHub for final code and acceptance review. Retrying an already completed `swarm issue submit <issue>` returns the existing PR rather than refreshing its description. To change that PR, use its PR number with the commands below; issue commands continue to take issue numbers.

### Continuing an Existing PR

Use the same verbs for the same kind of work: `issue work` implements an approved issue, while `pr work` continues its proposed implementation. `issue stage` and `pr stage` both record interactive plans or implementation checkpoints, but PR staging reads only the separate PR worktree and leaves the original issue branch unchanged. Fixing a failing test or addressing feedback is development, not a separate `repair` operation. `pr rebase` changes branch ancestry and does not ask an agent to rewrite the implementation.

PR commands require an open PR recorded in an approved WorkUnit's authoritative state. A closing reference in a PR description helps SwarmOS find that link quickly, but the link itself always comes from coordination state.

Who can revise a PR:

- **Maintainers** with upstream write permission can revise any linked PR. For a PR whose head lives in a contributor's fork, add a writable Git remote for that fork and select it with `--fork-remote <name>` (default `fork`), without `--fork`.
- **Contributors** revise their own fork PRs with `swarm --fork pr ...`. Changes publish to the branch in their fork, and the upstream fork coordinator records each step after checking that they own that fork, that the published head is the one they validated, and that the revised plan is approved. See [Revising your PR](fork-mode.md#revising-your-pr-after-review).

`pr status` shows the published head and target, local revision, check runs, and GitHub review records with their commit SHAs. Checks on the published head are separate from local `pr validate` results; reviews against an older commit remain visible as history. When the tracker reports no CI checks (for example, on the local backend), `pr status` shows the SwarmOS validation recorded for the published head as a `swarm-validation` check.

If commits are pushed to the PR branch outside SwarmOS, `pr status` reports `unverified-head`: the recorded plan, validation, and provenance attestation describe the last verified head, not the current one. Run `swarm pr work <PR>` to start a revision from the current head; it re-plans, re-validates, and records new evidence before `pr submit`.

```bash
# 33 is the PR number; its linked WorkUnit may have a different issue number.
swarm pr status 33 --backend github
swarm pr work 33 --dry-run --backend github
swarm pr work 33 --instructions "Fix the failing checks within the approved scope" --backend github
swarm pr validate 33 --backend github
swarm pr submit 33 --backend github
```

Changes are prepared in `.swarm/worktrees/pr-33`, separate from the original issue worktree. A new revision goes through planning and the configured review gate again. For a human gate, an independent maintainer approves the linked **issue's** plan with `swarm issue review-plan <issue> --approve`, then the worker resumes `swarm pr work <PR>`. Use the existing `--agent`, role-agent overrides, `--phase`, `--headless`, `--progress`, and `--json` flags as appropriate.

For `plan_mode: interactive`, `swarm pr work <PR>` pauses at `awaiting-plan` after preparing `.swarm/worktrees/pr-<PR>`. Write `.swarm/active/PLAN.md` and `.swarm/active/TEST_PLAN.md` **in that PR worktree**, then run `swarm pr stage <PR> --phase plan --harness <name> --model <name>` (optionally `--handoff` or `--json`). Under an agent gate, resume with `swarm pr work <PR>` to review and implement; under a human gate, an independent maintainer uses `swarm issue review-plan <issue> --approve` before `swarm pr work <PR>` resumes implementation. Do not stage through `swarm issue stage` against the old issue worktree while the PR revision is pending.

Alternatively, after the plan gate, make implementation changes in the managed PR worktree and run `swarm pr stage <PR> --phase implement --harness <name> --model <name>`. This records a local checkpoint and validates against the approved baseline; it does **not** publish the branch or change the submitted issue checkpoint. Inspect with `swarm pr status <PR>` and publish only with `swarm pr submit <PR>`. Staging requires the existing pending revision, a valid active issue claim, and upstream mutation authorization (or, with `--fork`, a claim on your own fork PR); it rejects rebases, diverged heads, and unrelated worktrees.

Issue and PR `validate` and `submit` commands report phases, check starts, and PASS/FAIL results with elapsed time on stderr. A running line appears every 15 seconds during a check or phase. Add `--progress=false` for quiet operation; stdout, PR URLs, and `--json` results stay the same. Issue submission creates a checkpoint and creates or updates its draft PR; PR submission publishes the existing checkpoint and updates review evidence and rebase metadata.

`pr submit` reruns trusted validation and publishes to the existing PR branch. It preserves the PR's title, human-written description, draft/ready state, and identity, and updates a managed rebase context section; GitHub applies the repository's review rules to the new commits. Publication uses the remote head observed when the revision began; a concurrent remote update is rejected rather than overwritten. Same-repository publication updates the PR branch and issue coordination state atomically.

#### Rebasing onto an Updated Base

The target is the existing PR's base branch, pinned to a full commit. When that branch moves ahead of the WorkUnit's approved baseline, a maintainer re-approves the issue against the new base before rebasing. Approval can't change while a claim is active, including your own claim from earlier `issue work`, so release it first. The next PR command claims the issue again automatically.

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

Use `--continue` or `--abort` only when a rebase is in progress. An approved plan carries forward when the WorkUnit and the committed `.swarm/` policy tree are unchanged, the previous baseline is an ancestor of the new one, and the review recorded that policy tree. In that case, run `swarm pr validate 33` and `swarm pr submit 33` right after rebasing. Implementation and validation evidence always need renewal, and a human gate still requires an authenticated human review.

When a plan can't carry forward, `pr status` explains why, lists the changed policy paths when available, and points to `swarm pr work <PR>`, which starts a new plan cycle. Reviews recorded before policy tree binding need a fresh review after a baseline move.

`pr status` and the submitted PR description show the old and new heads and bases, the paths that had conflicts, and the aggregate Git patch identity. Unchanged patch identity helps reviewers focus on conflict resolutions; trusted validation is still required either way. In fork mode, conflict paths are labeled as contributor reports, and the coordinator checks patch identity from the published commits. While a PR revision is pending, use the PR commands against its separate worktree rather than issue work/stage/validate/submit against the original issue worktree.

> [!TIP]
> If a rebase step stops with one of these errors, the fix is usually one command:
>
> | Message | What to do |
> | --- | --- |
> | `PR base tip differs from approved issue baseline` | Run `swarm issue release <issue>` if you hold a claim, then `swarm issue approve <issue>`, then retry the rebase. |
> | `issue is already claimed by another active lease` (from `issue approve`) | The message names the claim's owner and expiry. If it's yours, run the `swarm issue release <issue>` command it prints; otherwise wait for the claim to expire or ask its holder to release it. |
> | `rebase PR head … (conflicts preserved)` | Resolve the conflicts in `.swarm/worktrees/pr-<PR>`, `git add` each resolved file, then run `swarm pr rebase <PR> --continue`. |
> | `git rebase --continue: … needs merge` | Some files still have conflict markers or aren't staged. Check `git -C .swarm/worktrees/pr-<PR> status`, finish resolving, `git add` them, and continue again. |
> | `rebase already in progress` | Finish the paused rebase with `--continue`, or use `--abort`; don't start a new one. |
> | `PR worktree must be clean before starting rebase` | The PR worktree has uncommitted changes. Check `git -C .swarm/worktrees/pr-<PR> status`, keep them with `swarm pr stage <PR> --phase implement` or remove them, then retry. |

You don't need a separate command to resume a PR after its claim is lost or expires. `pr work`, `pr rebase`, `pr validate`, and `pr submit` each check the issue's lease first. Each one resumes your active claim when this machine holds its token, or takes a fresh claim when the old one has lapsed. `swarm pr status <PR>` shows where the revision stands.

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

By default, `swarm node run` drains the currently available ready queue and exits cleanly. Add `--watch` (and optional `--poll-interval 30s`) when you want the worker pool to keep polling for newly approved tasks.

---

## Related Guides

- **[Day in the Life of a Repo Admin (`docs/ADMIN_GUIDE.md`)](ADMIN_GUIDE.md)** — Roadmap decomposition (`swarm repo plan`), backlog triage, and Draft PR auditing.
- **[GitHub Security, Branching & Agent Authorization (`docs/GITHUB.md`)](GITHUB.md)** — Branch rulesets, `SWARM_GITHUB_TOKEN`, and collaborator allowlists.
- **[Project & Maintainer Setup (`docs/project.md`)](project.md)** — Initialize `.swarm/`, configure agents and checks, and approve tasks.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Architecture (`docs/ARCHITECTURE.md`)](ARCHITECTURE.md)** — Git-CAS state refs, fencing, and checkpoint mechanics.
- **[Security Policy (`SECURITY.md`)](../SECURITY.md)** — Trust boundaries, reviewer isolation, and release verification.
