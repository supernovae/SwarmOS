# SwarmOS Architecture

SwarmOS is built around a simple premise: **your Git repository and GitHub Issues already provide everything needed to coordinate multiple coding agents—without running an external coordination server or database.**

```mermaid
flowchart TB
    subgraph ControlPlane["Git & GitHub Control Plane"]
        Issue["GitHub Issue #N<br/>WorkUnit Contract"]
        Ref["Git CAS Ref<br/>refs/heads/swarm-state/issue-N"]
        Comment["Living Issue Comment<br/>Human-Readable Audit Projection"]
        Branch["Checkpoint Branch<br/>swarm/issue-N-slug"]
    end

    subgraph Execution["Local or Remote Worker Host"]
        WT["Isolated Worktree<br/>.swarm/worktrees/issue-N"]
        Sandbox["Reviewer Snapshot Sandbox<br/>Read-Only + Output-Only REVIEW.md"]
        Validator["Baseline Validator<br/> Policy from Approved Base SHA"]
    end

    Issue -->|swarm issue approve| Ref
    Ref -.->|Projected for humans| Comment
    Ref <-->|Fenced CAS claim & heartbeat| WT
    WT -->|Agent gate| Sandbox
    WT -->|Checkpoints| Branch
    WT --> Validator
```

---

## 1. Git Compare-and-Swap Coordination (`internal/coordination/` & `internal/lease/`)

SwarmOS stores authoritative task coordination state in dedicated Git refs (`refs/heads/swarm-state/issue-<N>` on GitHub, or a file-locked CAS store when `--backend local` is explicitly selected) and projects a human-readable summary comment onto the GitHub Issue.

Each state transition uses **Git compare-and-swap (`git push --force-with-lease`)** so concurrent workers serialize cleanly:

- **Immutable Approval Binding**: `swarm issue approve <issue>` verifies the caller's repository permission, parses the `WorkUnit`, resolves the current default-branch base commit, and records the `WorkUnit` digest, `.swarm/` policy digest, and `base_sha` in the state ref before applying `swarm:ready`.
- **Fenced Claims & Heartbeats**: Claiming a task generates a unique `claim_id` fencing token with a renewable TTL. Every subsequent artifact publication, branch checkpoint, renewal, and PR submission verifies that the caller still holds the active `claim_id` and that the task/policy digests have not changed.
- **Stale-Worker Protection**: If a worker loses its lease or a heartbeat renewal fails, its context is canceled immediately so a delayed process can never overwrite a newer claimant's state or branch.

---

## 2. Isolated Worktrees & Transferable Checkpoints (`internal/workspace/`)

Each claimed issue runs inside its own linked Git worktree at `.swarm/worktrees/issue-<N>` on branch `swarm/issue-<N>-<slug>`, pinned to the approved base commit:

- **Compiled Active Bundle**: SwarmOS generates `.swarm/active/WORKUNIT.md` and `.swarm/active/SYSTEM_PROMPT.md` inside the worktree, adds `.swarm/active/` to the worktree's `.git/info/exclude`, and injects managed pointer blocks into `AGENTS.md` and `CLAUDE.md`.
- **Atomic Branch Checkpoints**: On handoff or phase completion, SwarmOS strips temporary harness markers, commits the worktree state without rewriting history, and pushes both the checkpoint branch and the updated `swarm-state/issue-<N>` ref together. Another teammate or worker node pulling the handed-off task fetches and verifies that exact checkpoint SHA before continuing.
- **Dependency Ordering**: When a `WorkUnit` declares `depends_on`, SwarmOS waits until prerequisite issues are merged/closed (or share an explicitly verified stacked base/checkpoint lineage) before treating the task as ready.

---

## 3. Plan-Bound Gates & Baseline Validation (`internal/workflow/` & `internal/validation/`)

- **Cryptographically Bound Plan Reviews**: Both automated agent reviews (`--gate agent`) and maintainer approvals (`--gate human`) record the hashes of `WORKUNIT.md`, `PLAN.md`, and `TEST_PLAN.md`. Modifying the plan or issue automatically invalidates the prior approval so unreviewed plan changes cannot slip into implementation.
- **Trusted Baseline Policy**: During `swarm validate` and `swarm submit`, SwarmOS loads `.swarm/validation/required-checks.yaml` directly from the **approved base commit** (`base_sha`) rather than the candidate worktree. It diffs all committed, staged, working-tree, and untracked changes against `protected_paths` before running each required check in a sanitized environment.

---

## 4. Process & Reviewer Isolation (`internal/sandbox/` & `internal/agent/`)

### Design decision: use the harness's security model first

Users bring their agents and sign in through those agents' normal login flows. The harness owns provider authentication, sessions, refresh, and model connectivity. SwarmOS owns task coordination, repository policy, artifact acceptance, and publication.

Choose the simplest supported execution path that meets the project's policy:

1. Use the harness's native authentication, permissions, and sandbox.
2. Keep the authenticated harness in the user's environment and delegate tools to a restricted executor through a supported interface.
3. Use a disposable container with a documented, restricted authentication integration when the harness needs an external boundary.

Harnesses and their underlying OS controls are trusted dependencies. Each adapter documents its supported versions and platforms, effective permissions, tool coverage, and cancellation behavior. Focused checks cover the boundaries the project requires, including shell commands, file tools, subprocesses, and enabled MCP tools or plugins. Add an external executor only to cover a specific gap. A permission prompt alone does not establish isolation.

The two user-facing modes remain `isolated` and explicit `trusted-local`. `isolated` describes the enforced boundary, regardless of which supported harness or runtime provides it. The Codex native path uses the existing provider login. SwarmOS will not add a universal credential store, OAuth lifecycle, or model proxy.

### Implemented backend selection

`execution.backend` defaults to `native`. The first qualified adapter is Codex 0.163.0 on Linux. SwarmOS runs the authenticated harness in the user's environment and delegates execution to Codex's native sandbox. `swarm doctor` verifies the version, login availability, and actual filesystem/network restrictions. An unsupported adapter or failed probe stops the run before claiming work.

| Approach | Fit and tradeoff |
| --- | --- |
| **Trusted harness with native tool isolation — selected** | Reuses normal Codex login and session renewal. Codex enforces tool permissions; SwarmOS supplies task snapshots, runs boundary checks, and accepts bounded artifacts. The host-side harness and its internal tool dispatcher remain trusted. |
| **Trusted harness with an external executor** | Useful when a supported delegation interface covers a missing native capability. Each adapter must cover built-in tools as well as extensions. No separate external-executor adapter is implemented yet. |
| **Containerized harness with restricted authentication** | Useful for independently enforced isolation and resource quotas. The OCI executor is available for offline agents; provider-supported login, refresh, and revocation need qualification before authenticated use. |

Codex's documented CLI permission profiles provide the first complete path without a new transport or credential service. App-server is another supported integration interface, but is unnecessary for this slice. MCP alone would leave built-in tools uncovered. See the provider's [permissions](https://learn.chatgpt.com/docs/permissions) and [app-server](https://learn.chatgpt.com/docs/app-server) documentation.

- **Execution:** Codex shell and file tools use a restricted native profile. MCP, plugins, hooks, browser tools, and subagents are disabled. Validation uses the same harness's `sandbox` command without provider credentials. Interactive execution streams `codex exec` and accepts follow-up turns under the same permissions.
- **Files and authority:** Workers receive a detached snapshot without live Git metadata, coordinator state, or autoloaded `.codex`/`.agents` directories. Planner and reviewer input is read-only. SwarmOS imports only expected role artifacts or bounded developer/tester changes, then performs the existing provenance, policy, lease, and approval checks.
- **Credentials:** Codex opens its normal login store; SwarmOS neither reads nor copies provider tokens. Tool processes receive a clean environment and private home. Publishing credentials stay with the coordinator. Explicit `trusted-local` launches retain their existing allowlisted model-environment setup.
- **Optional OCI:** Approved repository policy can select `execution.backend: oci`. Existing explicit container settings also retain that selection. Native execution cannot silently replace a required container boundary. See [execution modes](../SECURITY.md#execution-modes) for setup and [future work](../todo.md#oci-authentication-and-enterprise-integrations) for authenticated OCI qualification.

Native capability tests cover host-file canaries, environment filtering, read-only roles, network denial, artifact import, and cancellation of a detached subprocess. The reproducible authenticated WorkUnit test exercises all four roles, interactive planning, unattended implementation, human approval, native validation, and a local Draft PR. Version expansion, macOS/Windows, provider session-expiration scenarios, and additional tool integrations remain qualification work.

Exact qualified releases live in a small adapter allowlist. Runtime checks return the tested version for diagnostics and artifact provenance. Each added release follows the [qualification procedure](native-qualification.md#adding-a-codex-release); repository policy can continue selecting an older installed binary.

---

## Related Guides

- **[Project & Maintainer Setup (`docs/project.md`)](project.md)** — Initialize `.swarm/`, configure agents, and approve WorkUnits.
- **[Developer & Worker Guide (`docs/developer.md`)](developer.md)** — Everyday CLI workflows, handoffs, and recovery.
- **[Workflows & Review Gates (`docs/WORKFLOWS.md`)](WORKFLOWS.md)** — Role prompts, artifacts, and gate transitions.
- **[Future Roadmap (`todo.md`)](../todo.md)** — Optional future coordinator service and planned enhancements.
