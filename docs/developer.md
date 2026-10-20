# Developer, Team & Worker Node Guide

This guide covers how individual developers, collaborating teammates, and autonomous worker nodes use `swarm` to discover, pull, execute, hand off, and submit work across one or more repositories.

---

## 1. Working in a Single Repository

### Pulling the Next Available Task
You don't need to browse GitHub Issues to find an unclaimed task. Run `swarm pull` or `swarm work --next` inside your repository:

```bash
# Find, claim, and provision the oldest unclaimed `swarm:ready` issue into .swarm/worktrees/issue-<N>
swarm pull --agent claude

# Or claim and immediately execute the full Plan -> Review Gate -> Implement -> Validate workflow
swarm work --next --agent claude --headless
```

What happens when you claim or pull an issue:
1. **Trust & Dependency Check**: Verifies the issue satisfies the repository's trust policy (`swarm:ready` label or trusted author) and that any `depends_on` prerequisite issues are complete.
2. **Optimistic Lease Acquisition**: Posts a living orchestration state comment on the issue with a 4-hour renewable TTL (`swarm:claimed`, `swarm:in-progress`). If two developers claim the same issue simultaneously, lowest GitHub `CommentID` wins and the runner-up automatically moves to the next ready issue.
3. **Isolated Git Worktree**: Creates branch `swarm/issue-<N>-<slug>` in `.swarm/worktrees/issue-<N>` without touching your main working directory.
4. **Universal Harness Compilation**: Writes `.swarm/active/WORKUNIT.md` and `.swarm/active/SYSTEM_PROMPT.md`, injects context pointers into `AGENTS.md` and `CLAUDE.md`, and installs `/swarm-plan`, `/swarm-review`, `/swarm-implement`, and `/swarm-validate` slash commands.

---

## 2. Interactive vs. Headless Agent Execution

SwarmOS supports **Claude Code (`claude`)**, **OpenAI Codex (`codex`)**, **OpenCode (`opencode`)**, and **OhMyPi (`omp`)**:

```bash
# Interactive mode (default): opens the agent TUI inside the isolated worktree with the compiled prompt
swarm work 12 --agent claude

# Headless mode (--headless): runs non-interactively and captures artifacts automatically
swarm work 12 --agent omp --headless
```

### Routing Roles to Specialized Agents
You can assign different agents to different workflow steps in a single run:

```bash
swarm work 12 \
  --planner-agent claude \
  --reviewer-agent omp \
  --developer-agent codex \
  --tester-agent opencode \
  --gate agent \
  --headless
```

1. **Planner (`--planner-agent`)**: Inspects the codebase without modifying production files and produces `.swarm/active/PLAN.md` (`design` artifact) and `.swarm/active/TEST_PLAN.md` (`test-plan` artifact).
2. **Reviewer (`--reviewer-agent`)**: Audits the plan against all constraints and acceptance criteria, writing `.swarm/active/REVIEW.md` (`VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED`).
3. **Developer (`--developer-agent`)**: Implements `PLAN.md` step-by-step and records an `implementation` artifact summarizing modified files.
4. **Tester (`--tester-agent`) & Validator**: Verifies acceptance criteria, checks protected paths for tampering, and runs `.swarm/validation/required-checks.yaml` (`validation` artifact).

---

## 3. Collaborative Team Handoffs (`--handoff`)

On a team, one contributor (or senior architect) may want to generate and vet the design plan, then hand the issue off for another contributor or coding node to implement:

```bash
# Contributor A: Run the plan step and release into collaborative handoff mode
swarm work 12 --phase plan --handoff --agent claude --headless
```

- Publishes the `design` and `test-plan` artifacts to the issue's state comment.
- Transitions the lease to `status: handoff` with `next_phase: implement` (or `review` if a review gate is pending) and records Contributor A in `contributors`.
- Removes `swarm:in-progress` so the next teammate or worker node can pick it up.

```bash
# Contributor B: Pulls the next available task (automatically claims the handed-off implement step!)
swarm work --next --agent codex --headless
```

Contributor B's worktree receives Contributor A's `PLAN.md` and `TEST_PLAN.md` automatically, and both contributors are credited in the lease's `contributors` list.

---

## 4. Validating & Submitting a Draft PR

Once implementation is complete:

```bash
# Run protected-path tamper checks and all commands in .swarm/validation/required-checks.yaml
swarm validate 12

# Clean temporary harness markers, commit & push the branch, and open an artifact-backed Draft PR
swarm submit 12
```

If you need more time or want to step away from an issue:
```bash
# Extend lease TTL by 4h (up to max_renewals)
swarm renew 12 --duration 4h

# Release the issue back to the backlog
swarm release 12 --reason "Stepping away"
```

---

## 5. Multi-Repository Setup (`swarm repo`)

If you contribute to multiple projects, register them in your local SwarmOS registry (`~/.config/swarm/repos.yaml`):

```bash
# Register an existing local checkout (verifies git origin remote matches owner/repo)
swarm repo add supernovae/SwarmOS ~/src/SwarmOS

# Or auto-clone a repository from GitHub into ~/.swarm/repos/<owner>/<repo> and register it
swarm repo add supernovae/AnotherProject --clone

# List all registered repositories and their harness status
swarm repo list

# View a unified dashboard of open issues, active claims, and TTLs across ALL registered repos
swarm status --all

# Pull or work on the next ready issue across all registered repos from any directory
swarm pull --all
swarm work --next --all --agent claude --reviewer-agent omp --gate agent --headless
```

### Logical Safety Guarantees Across Repos
- **Remote-Slug Verification**: `swarm repo add` checks `git remote get-url origin` and refuses to associate an `owner/repo` slug with a mismatched local checkout.
- **Location-Independent `--repo`**: Once registered, passing `--repo owner/repo` from anywhere on your machine automatically resolves the repository's local checkout path and provisions worktrees inside `<repoRoot>/.swarm/worktrees/issue-<N>`.
- **Non-Destructive Fetch**: Before provisioning a worktree, `swarm pull` runs `git fetch origin --prune` without altering whatever branch or uncommitted changes you have in your primary checkout.

---

## 6. Running Autonomous Worker Nodes (`swarm node`)

To donate compute/agent capacity or run background worker pools across your repositories:

```bash
# 1. Detect host OS/Arch/CPUs/RAM and installed agent CLIs (~/.config/swarm/node.yaml)
swarm node init --capabilities coding,testing,review,docs
swarm node status

# 2. Drain all currently ready work units across registered repos using 4 parallel workers
swarm node run --all --concurrency 4 --once --gate agent --auto-submit

# 3. Or run continuously as a polling daemon (checks every 30s)
swarm node run --all --concurrency 2 --once=false --poll-interval 30s --gate agent --auto-submit
```

Nodes automatically match their declared `capabilities` against each issue's `required_capabilities` (and handoff phase requirements) before claiming work.
