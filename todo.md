# Future work

## Optional trusted coordinator / admin CLI

Git compare-and-swap state refs are the implemented and authoritative coordination mechanism. Do not add a second state authority or silently fall back from Git CAS.

A future **optional** trusted coordinator or admin CLI may be considered only if Git-CAS operational evidence shows a need for central scheduling or recovery. It is not implemented.

Required design contract before implementation:

- The coordinator is explicitly enabled per repository and has a distinct, authenticated operator identity; ordinary workers and issue commenters cannot invoke administrative mutations.
- Git CAS remains the source of truth, or the coordinator has a documented, atomic claim-store contract with fencing tokens, durable attempt IDs, revision checks, and an auditable recovery path. There must never be split authority between comments, a database, and refs.
- Authorization distinguishes task approval, human plan approval, state mutation, release/reassignment, and submission. Every mutation is bound to repository, issue, task digest, policy digest, base SHA, claim ID, and current state version.
- The claim store provides compare-and-swap semantics, expiry, renewal, fencing of stale workers, idempotent mutation/submission, and deterministic crash recovery. It must preserve a successor's checkpoint.
- Publishing credentials remain with the trusted coordinator; agent and validation processes receive only the minimum explicitly permitted credentials.
- The CLI exposes read-only inspection and audit output, never an undocumented bypass of repository policy or human gates.

Acceptance evidence: concurrent same-owner and different-owner claims permit one executor; delayed/stale workers cannot publish or submit; approval and validation are bound to the exact task/plan/test/base/head revisions; crash/restart recovers without overwriting a successor; unauthorized users cannot administer state; all state transitions are auditable; and Git-CAS versus coordinator behavior is documented without claiming a feature that does not exist.

## Planned optimization work

Beyond the implemented `--max-jobs` limit, these remain planned rather than guaranteed: wall-time/cost budgets, repository fairness, richer structured event streams, and provider capability diagnostics. Cost limits require provider usage data; elapsed time must not be represented as a cost estimate.

## OCI authentication and enterprise integrations

Harness-owned authentication is the everyday path. OCI remains an optional boundary for teams that need independently enforced worker isolation and resource quotas. Complete these steps before advertising provider-authenticated OCI execution:

- Qualify a provider-supported login/session integration for each containerized harness, including renewal, expiry, cancellation, and revocation. Keep host credential stores and coordinator publishing tokens outside workers; avoid undocumented session-file copying or a general model proxy.
- Define credential scope and lifetime, permitted destinations/operations, and usage limits. Document which forms of model-account misuse remain possible even when tokens are hidden.
- Offer optional enterprise workload identity or an organization's OIDC integration where it serves a concrete deployment need. Keep provider authentication and worker identity separate.
- Evaluate repository-installed GitHub Apps for short-lived, scoped coordination/publishing authorization. Keep GitHub App installation optional and separate from OIDC federation.
- Run real Docker/Podman adversarial tests on Linux, macOS, and Windows: filesystem and credential access, egress, cancellation, detached processes, cgroup/resource limits, and safe artifact import. Record exact runtime and image versions.
- Document the stronger OS/resource guarantees and residual trust in the runtime, host, harness, and provider integration. `swarm repo doctor` must report authenticated OCI as unavailable until qualification passes.

## Native adapter qualification

- Expand the tested Codex version/platform matrix, including macOS and Windows. Exercise session expiration, renewal failure, cancellation during model/tool work, and checkpoint resumption without weakening repository policy.
- Expand Claude's restricted-mode integration with native file/shell capabilities where their controls cover the same boundary. Test hooks, MCP, and subprocesses before enabling additional execution paths.
- Complete authenticated OpenCode 2 and mixed-harness workflow qualification using operator-selected models. The implemented OpenCode MCP and OhMyPi RPC paths use isolated task tools; qualify safe native delegation, extensions, and session recovery incrementally.
- Add opt-in network/tool integrations and locally installed toolchain access through bounded repository policy as they are qualified. Preserve the simple default and fail closed on unsupported requirements.

## Proposed: adopt dependency PRs as draft WorkUnits

`swarm repo plan --scan` already sorts Dependabot and Renovate PRs into a **Dependency updates** lane. A follow-up could let `--scan --apply` propose one `swarm:draft` WorkUnit that bundles related open dependency PRs (for example, "Upgrade GitHub Actions: checkout 4 to 7, setup-go 5 to 7"), with acceptance criteria and the PRs it replaces. Collect real usage of grouped Dependabot PRs and the lane before deciding.

Required design contract before implementation:

- Build the WorkUnit only from GitHub-recorded metadata: the bot author login and the structured `bump <package> from <old> to <new>` title. Never copy PR bodies, commit messages, or release notes; that text is written by upstream package authors and would be a prompt-injection path into agents.
- The proposal is a draft. A maintainer approves it like any other WorkUnit; nothing is claimed, merged, or closed automatically.
- Updates that touch protected paths are flagged so the maintainer decides on `allow_harness_changes: true` explicitly. Never set it automatically.
- Re-running the scan updates the existing draft instead of creating duplicates, and skips dependency PRs that are already closed or adopted.

Acceptance evidence: the proposal lists the right packages and versions from titles alone; a malicious PR body has no effect on the WorkUnit; protected-path updates are flagged but not authorized; repeated scans are idempotent.
