# Lead a WorkUnit from your own session

Use human-led work when you want to explore the code yourself, pair with your preferred harness, and choose each edit while keeping a reviewable contract and validation record. SwarmOS prepares a Git worktree and records your staged phases; you stay in control of the session.

Your own interactive harness runs with your host permissions. Keep its approval prompts enabled. A worktree separates Git checkouts; it does not sandbox your harness. SwarmOS loads validation policy from the approved base, and configured agent reviewers run in an isolated read-only snapshot. See [reviewer isolation](developer.md#reviewer-isolation-and-credentials) for setup.

## Start an issue

From the registered repository, claim an approved issue and use the worktree path printed by the command. These examples use issue 83:

```bash
swarm issue claim 83
cd .swarm/worktrees/issue-83
swarm issue status 83
```

Open your interactive harness in that worktree. Read `.swarm/active/WORKUNIT.md` and `SYSTEM_PROMPT.md`. Run `/swarm-plan` where the harness provides it; otherwise ask the session to write `.swarm/active/PLAN.md` and `TEST_PLAN.md` following the active contract. Stage the files with the harness and model you used:

```bash
swarm issue stage 83 --phase plan --harness codex --model default
```

Staging records the authenticated stager and marks your harness and model as self-reported. Reviewers see this per role in status and PR evidence alongside the provenance attestation.

## Review the plan

Follow the WorkUnit's configured gate:

- **Agent:** request the configured sandboxed review with `swarm issue review-plan 83`. Address changes requested before implementing.
- **Human:** an independent authorized maintainer runs `swarm issue review-plan 83 --approve --notes 'Reviewed plan and test plan'`. Owning the claim does not authorize self-approval.
- **None:** continue after staging; no plan review is required.

Repositories with `plan_mode: agent` require agent-generated planning and disable manual plan staging. Use their configured planning workflow before continuing hands-on implementation. Fork contributors ask a maintainer to complete the authoritative review; a contributor's advisory review does not satisfy a gate.

## Implement and submit

Run `/swarm-implement`, or ask your session to implement the approved plan and run its focused tests. Keep edits in the worktree. Validate against trusted-base checks, then record the implementation and validation:

```bash
swarm issue validate 83
swarm issue stage 83 --phase implement --harness codex --model default
```

Use `swarm issue status 83` to inspect your claim. During a longer session, renew explicitly before expiry, within the approved renewal allowance:

```bash
swarm issue renew 83
swarm issue submit 83
```

Submission publishes a Draft PR with role summaries, validation, and provenance. Review and acceptance continue on GitHub.

## Continue an existing PR

Use the PR number for its separate worktree. For PR 321 linked to issue 83, prepare a planning session without running implementation:

```bash
swarm pr status 321
swarm pr work 321 --phase plan
cd .swarm/worktrees/pr-321
```

Read the active contract and revision context, then use `/swarm-plan` or the equivalent prompt. Stage the revised plan in this PR worktree:

```bash
swarm pr stage 321 --phase plan --harness codex --model default
```

Complete the linked issue's configured review as described above, using `swarm issue review-plan 83` for an agent review or the independent maintainer approval command for a human gate. Then use `/swarm-implement` or an equivalent prompt to implement the approved revision.

```bash
swarm pr validate 321
swarm pr stage 321 --phase implement --harness codex --model default
swarm issue renew 83
swarm pr submit 321
```

Renewal and plan review belong to the linked issue. PR staging, validation, and submission use the PR number. Submission checks the expected remote head before publishing.

## Work from your own fork

For initial issue work, add `--fork` to each SwarmOS command above. For an existing PR, contributors can revise their own fork PR; prepare it with `swarm --fork pr work 321 --phase plan`. Keep `--fork` on subsequent status, stage, validate, renew, and submit commands too. Maintainers use the same-repository commands above with upstream write access. See [fork setup](developer.md) for remote configuration and coordinator setup.
