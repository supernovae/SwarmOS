# Security Policy

## Supported versions

The latest tagged release is supported according to its release notes. Always match your installed binary (`swarm version`) with the documentation for that release or commit.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/supernovae/SwarmOS/security/advisories/new), not a public issue. Include the SwarmOS version or commit, operating system, backend, configuration, and a minimal reproduction where safe to share.

## Threat model and enforced boundaries

SwarmOS keeps task approval, coordination state, and publication under maintainer-controlled policy. Task workers receive the execution boundary selected by that policy; the coordinator remains a trusted process.

Our [execution architecture](docs/ARCHITECTURE.md#design-decision-use-the-harnesss-security-model-first) builds on each supported harness's authentication, permissions, and sandbox. SwarmOS verifies the integration against repository policy and adds external isolation only for capabilities the harness cannot supply. Sign in through the harness and let SwarmOS coordinate the work.

| Owner | Responsibility | How it is checked |
| --- | --- | --- |
| Agent harness | Provider login, session renewal, model connectivity, and native tool permissions. | Adapter support for documented harness versions and settings; focused tool-boundary tests. |
| OS or container runtime | Enforce the filesystem, process, and network boundary used by the harness or executor. | Runtime capability checks and tests on supported platforms. |
| SwarmOS | Approved task scope, leases, artifact acceptance, review provenance, gates, and authorized publication. | Approved-base policy, fenced state transitions, and regression tests. |
| GitHub | Repository access, protected refs, merge rules, and release authorization. | Repository settings and checks for the submitted commit. |
| Operator | Choose trusted harnesses and extensions, authenticate with providers, and configure project policy. | `swarm repo doctor`, task previews, and review of enabled integrations. |

Provider login, worker identity, and GitHub authorization remain separate. Organizations may later integrate their own identity system or a repository-installed GitHub App for scoped automation credentials. These are optional integrations; a GitHub App supplies GitHub authorization, not an OIDC identity provider.

The enforced controls in the Git-CAS workflow are:

- A task is a draft until an authorized actor runs `swarm issue approve <issue>`, which persists the WorkUnit digest, base-bound policy digest, committed `.swarm/` tree identity, and trusted base SHA before marking it ready.
- State is read and written through authoritative Git CAS refs (or the explicitly selected locked local backend). Comments are audit projections and cannot create claims, approvals, or artifacts.
- Worker-owned claim, artifact, checkpoint, and submission mutations are fenced by a unique claim ID and check task/policy revision, expiry, and ownership. Authorized task approval and human review are independent CAS operations. A stale worker must stop rather than publish to a successor's task.
- Each claim and renewal lasts at most the approved base's `lease.duration`, in same-repository and fork mode alike. A longer `--duration`, or a longer duration in the checkout's config, is clamped to it; shorter requests are kept. `lease.max_renewals` and the total claim budget still apply unchanged.
- `swarm issue revoke <issue> --reason <text>` lets an actor with `approve-task` permission end an abandoned or hoarded claim on the github backend. It is refused in `--fork` contributor mode, without that permission, and when the issue has no active claim. One fenced CAS ends the captured claim and records the maintainer, reason, time, and revoked claim ID in authoritative state; for a fork claim, the same atomic push removes it from the contributor's `swarm-contributors` index, so it stops counting toward `fork.max_active_claims`. If the claim changed first, the revoke fails rather than end a successor's claim. Revoke never approves, edits, or publishes work: approval, artifacts, and checkpoints stay in state, and a same-repository checkpoint is resumed by the next claimer. A new claim after a revoked fork claim starts from the approved base, as with any fork takeover. The previous holder's next fenced write fails with a claim-fence error. The audit comment and the removal of `swarm:claimed` and `swarm:in-progress` are projections; if they fail, the revoke still stands and the command prints a warning.
- Plan approval is bound to the task, committed `.swarm/` policy tree, plan, and test-plan digests. An authenticated review can cross a baseline move only through a coordinator-owned transition that verifies unchanged task and policy content, a valid original review with tree metadata, and ancestry from the old approved base to the new one. Legacy reviews remain valid at their exact original base but cannot carry forward. Claim fencing keeps its separate base-bound policy digest. Implementation and validation evidence are cleared on every baseline move. Agent approval and human approval are distinct; a human gate cannot be satisfied by an agent verdict. Contributor-supplied review artifacts cannot establish trusted agent approval.
- A fresh same-repository issue claim can carry task approval to the current default tip through one coordinator CAS transaction that records the previous base and takes the claim. It requires unchanged task and committed policy tree digests and old-to-new ancestry; human gates require explicit approval of a moved base. With no plan artifacts, no review transition is created. Any existing plan or review evidence requires complete authenticated review carry. Carry compares only the committed `.swarm/` tree: changes to `AGENTS.md`, `CLAUDE.md`, `.claude/commands/**` or `.github/workflows/**` alone do not prevent carry.
- Routine `pr rebase --resolve` task reapproval is restricted to an authenticated actor with `approve-task` permission, an unchanged WorkUnit digest, identical committed old/new `.swarm/` policy trees, the expected prior approval and PR head/base, and no active claim. The coordinator saves the transition with CAS and projects an actor/base audit comment; projection failure can be retried without granting approval again. Changed policy or task content requires explicit task approval. Fork workers currently request that approval from a maintainer.
- After explicit approval of changed policy, a linked PR can retain its old plans as stale evidence. Only an isolated, configured reviewer can establish a fresh agent review of unchanged plans under the new bindings; the previous verdict remains invalid at the new base. Human gates and fork plan review require a maintainer decision.
- Validation obtains checks and protected paths from the approved base, not the candidate worktree. A candidate cannot remove validation policy to pass. A `**` path component in a protected pattern matches any number of directories, including none, so `**/.gitattributes` also protects the root file and deeply nested copies. A malformed trusted pattern fails validation, naming the pattern, instead of matching nothing.
- Configured agents are allowlisted by name, role, executable, and optional model/arguments. `mock` is allowed only for explicit local/test use.
- GitHub credentials stay with the coordinator. Agents receive neither the token nor `SWARM_GITHUB_TOKEN_COMMAND`. A token from that command lives only in `swarm`'s memory and the scoped Git child environment, never in argv or on disk. Only the operator's environment can set the command, never repository configuration. See [Keep tokens out of your shell](docs/GITHUB.md#keep-tokens-out-of-your-shell-with-a-secret-manager).
- PR revisions are linked through authoritative issue state, not PR description text. `pr work` and `pr rebase` prepare a separate local candidate; `pr submit` validates and publishes with an explicit expected remote head. A newer target baseline needs WorkUnit approval, and ordinary development needs fresh plan-review evidence. An ancestry-only rebase can retain an eligible authenticated plan, followed by fresh trusted validation and explicit submission. Maintainers revise PRs with upstream write access. Contributors revise only their own fork PRs through the fork coordinator, which verifies fork ownership, the published head, a passing validation report for that head, and maintainer plan approval before recording the revision. A PR head pushed outside SwarmOS is reported as `unverified-head`; its recorded evidence describes the last verified head, and upstream CI still validates every pushed head against the trusted base.
- `pr rebase --resolve` keeps Git with the coordinator. The developer agent runs through the same configured runner and boundary as `pr work`, chosen from the approved base's policy, on a disposable workspace that holds only the conflicted files, their index sides, the WorkUnit, the approved plan, and a compiled-in resolver prompt. It never contains `.git`, other repository files, or credentials, so a Git binary inside the sandbox can only act on that copy. Under explicitly configured `trusted-local` execution the agent has the same host reach as `pr work`; the snapshot guarantee is enforced by isolated execution. SwarmOS imports a result only when every change is limited to the conflicted files and no conflict markers remain, then stages it and continues the rebase itself. Protected paths (unless the WorkUnit allows harness changes), SwarmOS control paths, deletions, links, and special files stay paused for a person. A separate planning turn produces a bounded rebase micro-plan before each resolution; its body and digest are stored as rebase evidence, outside the plan-gate artifact types. After rebase, trusted-base checks run under the same fenced claim. Bounded repair uses a separate Git-free snapshot and a fixed path allowlist from the PR diff plus conflicts; source, mode, addition and deletion changes outside that set, control-file edits, links and special files are rejected before import. Accepted edits become local checkpoints, and the trusted-base repair budget survives retries. Recorded resolutions are review evidence only: they never satisfy a plan gate or count as validation, and fork contributors' resolutions are labeled as reports.

HTML comments are stripped before parsing an issue contract. This reduces one hidden-text input path; it does **not** prevent prompt injection in visible issue text, source files, tool output, dependencies, or an agent's external context. Treat all untrusted content as data and review generated plans and changes.

## Writable agent returns

Before importing developer or tester results, SwarmOS checks every added, changed, removed, or mode-changed file against protected paths from the approved base. Protected edits require an approved WorkUnit with `allow_harness_changes`. Fork runs retain the stricter prohibition on `.swarm/` and `.github/workflows/` changes, with exact role reports permitted.

Within `.swarm/active/`, developers may return only `IMPLEMENTATION.md` and `LESSONS.md`; testers may return only `VALIDATION.md` and `LESSONS.md`. Approved inputs remain restricted even with harness permission. Before testing, the coordinator restores the approved plan, test plan, and WorkUnit. These checks cover native Codex, harness-tools Bubblewrap, and OCI through their shared import boundary. Symlinks and special files are rejected before import.

A policy rejection imports none of the returned changes and creates no report artifact or checkpoint for that run. Status records the rejected paths and the command to retry implementation.

## Execution modes

SwarmOS has two execution modes. `isolated` is the default when no mode is configured. Choose a repository default with `execution.mode` in `.swarm/config.yaml`, or select a local mode with `SWARM_EXECUTION_MODE`. The approved base commit supplies the task's policy. Setting `security.require_isolation: true` prevents an operator from selecting `trusted-local` for that task.

| Mode | Worker boundary | Setup |
| --- | --- | --- |
| `isolated` | Codex native sandbox or restricted delegated tools for the selected harness; OCI when required by repository policy. Missing required capabilities block execution. | A supported Linux harness/runtime, its normal provider login, and passing `swarm repo doctor` checks. |
| `trusted-local` | Planner, developer, tester, and validation commands run with the operator's OS permissions. Reviewers retain their separate snapshot sandbox. | Select this mode explicitly for repositories and tools you trust, where approved policy permits it. |

Generated CI runs `swarm repo validate` on a CI runner (detected through `CI=true` or `GITHUB_ACTIONS=true`). There, the ephemeral runner is the isolation boundary: required checks run directly on it with a scrubbed environment, so stock runners need no agent harness such as Codex. Checks and protected paths still come from the trusted base commit, and fork pull requests receive GitHub's read-only token. On a workstation, `swarm repo validate` keeps using the repository's configured sandbox.

For a pull request opened by a maintainer (write, maintain, or admin access) from a branch in the repository, the generated workflow runs `swarm repo validate --protected-paths report`. Protected-path changes are listed in the job summary rather than rejected, and required checks still come from the trusted base. Branches named `swarm/...`, which SwarmOS opens on the maintainer's behalf, fork pull requests, and all other authors stay on strict enforcement. Agent work is also checked against protected paths before `issue submit` and `pr submit`, independent of CI. Pull request workflows run from the pull request's merge commit, so a pull request that edits the workflow changes what its own CI runs; `.github/workflows/**` is a protected path, and required reviews in branch protection are what catch that.

### Native Codex

The trusted Codex process uses its own provider login and session renewal. Agent tools run with a named permission profile: a detached task snapshot, private scratch space, and Codex's minimal system runtime files. Live Git metadata, coordinator state, and host credential stores are outside that profile. Shell environments exclude provider and publishing tokens; tool networking is disabled. Code execution and subprocess restrictions come from Codex's Linux sandbox. SwarmOS checks outside-file, symlink, write, and loopback canaries before running work.

SwarmOS uses `codex exec` with user execution settings and rules ignored, disables MCP, plugins, hooks, browser tools, and subagents, and excludes `.codex` and `.agents` directories from the task snapshot. Interactive turns use the same permissions as unattended turns; requests to escalate access are denied. Planner and reviewer source is read-only, with designated artifact output. Developer/tester changes pass through bounded file import, and validation runs approved-base commands through `codex sandbox` in a disposable snapshot.

Codex and its internal tool dispatcher remain trusted host-side software. The boundary protects against agent-directed tools, not a compromised harness binary or OS. Task context is sent to the chosen model through the authenticated harness; keep secrets out of task source. Native execution has a 60-minute phase deadline and bounded imports, but does not add OCI's CPU, memory, or disk quotas. Provider spend controls remain with the account owner. See the [Codex guide](docs/harness/codex.md) for setup and troubleshooting.

### Claude Code, OpenCode 2, and OhMyPi

These adapters keep the authenticated harness in a coordinator-owned control directory and route workspace reads, writes, edits, and shell commands through Bubblewrap. Each tool receives a detached task snapshot, read-only system tools under `/usr`, private temporary storage, a clean environment, and a separate PID/network namespace. Planner and reviewer source stays read-only. Validation uses the same executor without a provider session.

Claude uses its supported restricted mode and an explicit MCP tool server. OhMyPi registers host tools through its RPC interface and verifies the effective tool list before each turn. OpenCode 2 uses a private loopback server, waits for the MCP catalog, and checks the effective agent permissions before work starts. A temporary server password remains between SwarmOS and OpenCode; it is unrelated to provider authentication and never enters the tool environment. The operator's shared OpenCode server is not used.

Native built-ins that would bypass this executor, custom hooks, external MCP servers, plugins, and agent delegation remain restricted. These adapters trust the installed harness to enforce that tool selection; Bubblewrap independently confines the tools it receives. The [capability table](docs/ARCHITECTURE.md#tool-capabilities), [harness guides](docs/harness/README.md), and [qualification guide](docs/native-qualification.md) describe the tested coverage.

Tools have two-minute deadlines and per-process CPU, address-space, file-size, and descriptor limits. Private temporary storage is limited to 256 MiB per call. These limits do not provide aggregate memory or task-workspace disk quotas. Shared worker operators can apply service-level limits; the OCI backend supplies a separate resource boundary.

### Optional OCI backend

Set `execution.backend: oci`, `execution.container_runtime` to `docker` or `podman`, and `execution.container_image` to a locally available image pinned by SHA-256 digest. Existing policies that explicitly configure container settings retain OCI selection. Conflicting native/container requirements are rejected.

OCI currently supports **offline workers** with `agents.<name>.authentication: none`. Authenticated provider execution inside OCI is future work, separate from the working native Codex path. Host credential stores are never mounted or copied into these workers.

OCI workers have a read-only image, no host mounts, no network, dropped capabilities, temporary storage, and fixed CPU, memory, process, and wall-time limits. Runtime preflight checks effective cgroup v2 limits and the ability to pause workers. Changes return through bounded import after the container is frozen and destroyed. Agent executables resolve inside the image. The [OCI qualification tests](docs/harness/README.md#oci-backend-qualification) exercise these controls against a real Docker or Podman runtime.

A failed isolated run never falls back to host execution. Native macOS and Windows execution require runtime qualification; WSL2 can use the Linux path.

The [qualification guide](docs/native-qualification.md) lists each harness's tested coverage and what remains. Stable releases at or above each adapter's minimum must pass interface and runtime capability checks. The actual version is recorded in artifact provenance.

`swarm repo doctor` and `swarm issue work <issue> --dry-run` report execution setup and blockers. These checks help prepare the worker environment before acquiring work.

## Boundaries that need operator care

- Git CAS coordinates trusted writers; it does not cryptographically attest arbitrary state payloads. A principal able to push `refs/heads/swarm-state/*` can replace authoritative state outside the CLI. Restrict authoritative refs and checkpoint publication to trusted orchestration identities, including ruleset bypass privileges, and audit direct pushes.
- `trusted-local` shares the operator's OS account. Temporary HOME/XDG directories and filtered environment variables reduce accidental credential inheritance, but workers can still access other files and processes available to that account. Use a disposable, least-privilege worker account when selecting this mode.
- Reviewers use a separate snapshot and import only bounded `REVIEW.md` output. In `trusted-local`, their sandbox uses `sandbox-exec` on macOS, `bwrap` on Linux, or configured OCI containers; Windows requires the container path. The selected reviewer boundary must be available. Reviewer network access is denied by default; `agents.<name>.review_network: true` permits unrestricted egress only in compatibility mode. Native reviewer resource enforcement and descendant termination still need platform-specific validation.
- Internal state, WorkUnit files, scaffold destinations, and artifact imports use root-bound filesystem operations. Internal symbolic links and unsafe file types are rejected, and writes use exclusive temporary files followed by atomic replacement. The one exception is a repository's `AGENTS.md` or `CLAUDE.md` entrypoint: it may be a relative symlink, resolved one component at a time, when every hop stays inside the repository or task worktree, avoids `.git/` and `.swarm/`, and ends at a regular file. Init and task pointer updates then read and write that resolved file with the same root-bound operations, and any other instruction link is refused before a write. This protects coordinator file operations from path substitution; it does not turn a shared OS account into a separate identity.
- GitHub branch protection, repository permissions, and human PR review remain the merge authority. Review the exact submitted SHA before merging or releasing.
- The local backend is for offline development and tests. It does not replace GitHub authorization.
- Hosted Grok Bot and OpenAI Dot use their own account, connector, and computer permissions. SwarmOS does not provision those connections or convert their comments into plan approvals. Local entries named `grok` or `dot` must select the supported harness they launch.

## Known limitations

These are current boundaries of the implementation, tracked as future hardening:

- **Multi-file imports are not transactional.** Each imported file is replaced atomically, but a disk error partway through a developer or tester import can leave part of a change set applied. Acceptance and publication still have to succeed separately. Isolated imports do not carry empty-directory changes or symbolic-link outputs.
- **Comment-triggered workflows still allocate runners.** The fork coordinator runs admission checks before checkout or toolchain setup, but GitHub allocates a runner for every eligible comment event. Watch Actions usage on busy public repositories.
- **Fork plans need a maintainer decision.** Contributor-supplied review artifacts never satisfy a trusted agent gate, and trusted reviewers are not yet scheduled automatically for fork plans. A maintainer approves fork plans with `swarm issue review-plan <issue> --approve`.
- **Platform coverage.** Native isolated execution is qualified on Linux (including WSL2). Native macOS and Windows execution, and live provider session expiration, still need qualification. Authenticated harnesses inside OCI are on the [roadmap](todo.md#oci-authentication-and-enterprise-integrations).

## Regression coverage

Security fixes ship with adversarial regression tests that fail against the vulnerable behavior: symlink and path-substitution escapes in state, scaffold, and artifact writes; forged or stale approvals; contributor-supplied review artifacts; fork comment forgery and polling amplification; stale-claim publication; candidate edits to validation policy; and credential exposure through process arguments. Contributors should follow the [security invariants in AGENTS.md](AGENTS.md#5-security-invariants-do-not-regress) so changes keep these protections in place.

## Operational guidance

1. Commit strict `.swarm/config.yaml`, workflows, prompts, and validation policy to the default branch.
2. Use explicit GitHub backend configuration in production; use `--backend local` only when intentionally offline.
3. Create draft tasks, inspect them, then approve the exact revision. Reapprove after substantive issue or policy changes.
4. Use human plan gates for changes requiring maintainer judgment. Review `--dry-run` policy output before unattended work.
5. Configure and verify isolated execution on each worker platform. If you explicitly choose `trusted-local`, use a disposable, least-privilege account and keep publishing authority separate.
6. Require CI and branch protection for the exact submitted SHA before release or merge.

## Release administration

Release publication runs the exact tagged commit through the repository's reusable lint, vulnerability, and cross-platform test workflow and requires that release tag to be reachable from the default branch. Publishing uses the GitHub `release` environment. Administrators must configure required reviewers and protection for that environment and protect release tags/branches in GitHub; workflow YAML alone cannot enforce remote repository settings.

## Supply chain and release evidence

Release archives are built and attested in GitHub Actions after passing the lint, vulnerability, and multi-platform test matrix. You can verify the provenance of any release archive with the GitHub CLI:

```bash
gh attestation verify 'swarm_<version>_<os>_<arch>.tar.gz' --repo supernovae/SwarmOS
```
