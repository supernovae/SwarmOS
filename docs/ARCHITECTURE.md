# SwarmOS architecture

## Authority and consistency model

Git compare-and-swap refs are the authoritative task state store. For a GitHub task, state is stored on a dedicated `swarm-state/issue-<N>` ref; the local backend uses a durable locked CAS store for explicitly offline operation. GitHub issues and comments remain the human work surface and audit projection, but parsing a comment never authorizes a claim, approval, handoff, artifact, or release.

This is a serialized state machine, not a claim that GitHub comments alone provide distributed locking. Worker-owned claim, artifact, checkpoint, and submission mutations verify repository/task/policy identity and claim fencing before CAS writes. Authorized task approval and human review are independent CAS operations. A conflict is a retry/failure condition, never permission to overwrite a newer state.

The Git remote authenticates ref writers, not the semantic claims inside arbitrary JSON. Protect `refs/heads/swarm-state/*` and issue checkpoint branches: a writer bypassing the CLI can forge state. CAS supplies ordering and stale-write rejection for trusted operators, not signed human-approval attestation against a compromised coordinator.

Authoritative state binds:

- WorkUnit digest and policy digest;
- approved immutable base SHA;
- branch and checkpoint head SHA;
- unique claim ID, owner/node, expiry, renewal state, and phase;
- artifacts and hashes, including plan/test-plan/review approval binding;
- submission identity when a Draft PR is created.

## Task approval and claiming

`swarm issue create` is draft-first. `swarm issue approve` authenticates the actor, parses the current WorkUnit, resolves the trusted base, persists task/policy/base identity with CAS, then applies readiness. Changed issue content, policy, permissions, or readiness cannot silently resume an old authorization.

Acquire creates a unique attempt. The CLI persists its claim token outside the worktree and serializes same-machine execution. Every later state/artifact/checkpoint/submission operation verifies the live token, expiry, and current revisions. Heartbeat renewal failure cancels work. A stale process therefore cannot publish after a successor has claimed the task.

## Workspace and checkpoints

The base is `origin`'s advertised default-branch commit after fetch when a remote exists; otherwise it is local `HEAD` for an explicit offline fixture. Provisioning verifies an existing worktree is the expected linked worktree, branch, and checkpoint. It does not use branch-reset behavior to recreate one.

A checkpoint cleans ephemeral active markers, commits without rewriting history, and pushes the exact head for GitHub. The state records that head. A different machine fetches and verifies it before resuming, transferring code rather than merely transferring a textual implementation artifact.

Dependencies default to merged/closed prerequisites. A PR-open label is not proof that prerequisite code is present. Stacked dependencies require an explicitly supported base/checkpoint relationship.

## Plan and validation transitions

Workflow YAML is a role prompt recipe, not an executable graph. The fixed engine produces plan and test-plan artifacts, applies the effective gate, implements, validates, then leaves submission explicit.

Gate verification is shared at transition points. Agent approval requires an exact verdict and matching current WorkUnit/plan/test-plan hashes. Human approval requires tracker authorization for a human decision. Replacing a plan or test plan invalidates prior approval.

Validation policy is resolved from the approved base before candidate code runs. It compares base-to-candidate changes as well as working/index/untracked state, so candidate changes cannot erase their own protected-path rules or required checks. Submission checkpoints first, validates the exact clean head, then records validation and submission state; changed code invalidates earlier evidence.

## Process boundary

Worktrees isolate Git checkouts only. Agents and checks are host processes unless configured otherwise. Reviewer isolation is stronger: it runs a separate immutable snapshot with an output-only review artifact channel. The current implementation uses `sandbox-exec` on macOS, `bwrap` on Linux, or a configured OCI container runtime; Windows has no native host reviewer sandbox and requires the container path. An unavailable selected boundary fails closed. Linux `bwrap` uses user, IPC, PID, and UTS namespaces and isolates networking by default. Container review is reviewer-only with a read-only root, dropped capabilities/no-new-privileges, a read-only snapshot, and explicit writable output/scratch mounts; Linux Docker requires accessible bind mounts. Reviewer networking is denied by default; `review_network: true` permits unrestricted host/container egress for that reviewer and is not domain-filtered.

Agent children start with a temporary credential-free scratch home and do not inherit host `HOME`, XDG, SSH, Git, or publishing-token state; only exact configured environment entries are forwarded, and configuration rejects `GH_*`, `GITHUB_*`, `GIT_*`, `SSH_*`, `HOME`, and `XDG_*`. This environment filtering does not isolate planner, developer, or tester host processes: they share the user UID and can directly read host files or credential stores. Review snapshot creation rejects absolute or escaping symlinks, denies the original worktree and host home, and atomically imports only a bounded regular `REVIEW.md`. This architecture does not claim prevention of visible prompt injection, malicious repository code, network exfiltration, or compromised host credentials.

## Operational limits

Git CAS establishes state ordering, but operators still need repository permissions, remote availability, branch protection, safe credential management, and release CI evidence for the exact SHA. Beyond the implemented `--max-jobs` limit, wall-time/cost budgets, repository fairness, richer structured event streams, and provider capability diagnostics remain planned work. The optional future centralized coordinator is specified, not implemented, in [todo.md](../todo.md).
