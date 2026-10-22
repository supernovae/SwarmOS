# Security Policy

## Supported versions

The latest tagged release is supported according to its release notes. Always match your installed binary (`swarm version`) with the documentation for that release or commit.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/supernovae/SwarmOS/security/advisories/new), not a public issue. Include the SwarmOS version or commit, operating system, backend, configuration, and a minimal reproduction where safe to share.

## Threat model and enforced boundaries

SwarmOS keeps task approval, coordination state, and publication under maintainer-controlled policy. Task workers receive the execution boundary selected by that policy; the coordinator remains a trusted process.

Our [execution architecture](docs/ARCHITECTURE.md#design-decision-use-the-harnesss-security-model-first) builds on each supported harness's authentication, permissions, and sandbox. SwarmOS verifies the integration against repository policy and adds external isolation only for capabilities the harness cannot supply. For the native Codex integration, sign in normally and let SwarmOS coordinate the work.

| Owner | Responsibility | How it is checked |
| --- | --- | --- |
| Agent harness | Provider login, session renewal, model connectivity, and native tool permissions. | Adapter support for documented harness versions and settings; focused tool-boundary tests. |
| OS or container runtime | Enforce the filesystem, process, and network boundary used by the harness or executor. | Runtime capability checks and tests on supported platforms. |
| SwarmOS | Approved task scope, leases, artifact acceptance, review provenance, gates, and authorized publication. | Approved-base policy, fenced state transitions, and regression tests. |
| GitHub | Repository access, protected refs, merge rules, and release authorization. | Repository settings and checks for the submitted commit. |
| Operator | Choose trusted harnesses and extensions, authenticate with providers, and configure project policy. | `swarm doctor`, task previews, and review of enabled integrations. |

Provider login, worker identity, and GitHub authorization remain separate. Organizations may later integrate their own identity system or a repository-installed GitHub App for scoped automation credentials. These are optional integrations; a GitHub App supplies GitHub authorization, not an OIDC identity provider.

The enforced controls in the Git-CAS workflow are:

- A task is a draft until an authorized actor runs `swarm issue approve <issue>`, which persists the WorkUnit digest, policy digest, and trusted base SHA before marking it ready.
- State is read and written through authoritative Git CAS refs (or the explicitly selected locked local backend). Comments are audit projections and cannot create claims, approvals, or artifacts.
- Worker-owned claim, artifact, checkpoint, and submission mutations are fenced by a unique claim ID and check task/policy revision, expiry, and ownership. Authorized task approval and human review are independent CAS operations. A stale worker must stop rather than publish to a successor's task.
- Plan approval is bound to the current task, policy, plan, and test-plan digests. Agent approval and human approval are distinct; a human gate cannot be satisfied by an agent verdict. Contributor-supplied review artifacts cannot establish trusted agent approval.
- Validation obtains checks and protected paths from the approved base, not the candidate worktree. A candidate cannot remove validation policy to pass.
- Configured agents are allowlisted by name, role, executable, and optional model/arguments. `mock` is allowed only for explicit local/test use.

HTML comments are stripped before parsing an issue contract. This reduces one hidden-text input path; it does **not** prevent prompt injection in visible issue text, source files, tool output, dependencies, or an agent's external context. Treat all untrusted content as data and review generated plans and changes.

## Execution modes

SwarmOS has two execution modes. `isolated` is the default when no mode is configured. Choose a repository default with `execution.mode` in `.swarm/config.yaml`, or select a local mode with `SWARM_EXECUTION_MODE`. The approved base commit supplies the task's policy. Setting `security.require_isolation: true` prevents an operator from selecting `trusted-local` for that task.

| Mode | Worker boundary | Setup |
| --- | --- | --- |
| `isolated` | Native Codex permissions and sandbox by default; OCI when selected by repository policy. Missing required capabilities block execution. | Codex 0.163.0 on Linux, a normal `codex login`, and passing `swarm doctor` checks. |
| `trusted-local` | Planner, developer, tester, and validation commands run with the operator's OS permissions. Reviewers retain their separate snapshot sandbox. | Select this mode explicitly for repositories and tools you trust, where approved policy permits it. |

### Native Codex

The trusted Codex process uses its own provider login and session renewal. Agent tools run with a named permission profile: a detached task snapshot, private scratch space, and Codex's minimal system runtime files. Live Git metadata, coordinator state, and host credential stores are outside that profile. Shell environments exclude provider and publishing tokens; tool networking is disabled. Code execution and subprocess restrictions come from Codex's Linux sandbox. SwarmOS checks outside-file, symlink, write, and loopback canaries before running work.

SwarmOS uses `codex exec` with user execution settings and rules ignored, disables MCP, plugins, hooks, browser tools, and subagents, and excludes `.codex` and `.agents` directories from the task snapshot. Interactive turns use the same permissions as unattended turns; requests to escalate access are denied. Planner and reviewer source is read-only, with designated artifact output. Developer/tester changes pass through bounded file import, and validation runs approved-base commands through `codex sandbox` in a disposable snapshot.

Codex and its internal tool dispatcher remain trusted host-side software. The boundary protects against agent-directed tools, not a compromised harness binary or OS. Task context is sent to the chosen model through the authenticated harness; keep secrets out of task source. Native execution has a fifteen-minute phase deadline and bounded imports, but does not add OCI's CPU, memory, or disk quotas. Provider spend controls remain with the account owner.

### Optional OCI backend

Set `execution.backend: oci`, `execution.container_runtime` to `docker` or `podman`, and `execution.container_image` to a locally available image pinned by SHA-256 digest. Existing policies that explicitly configure container settings retain OCI selection. Conflicting native/container requirements are rejected.

OCI currently supports **offline workers** with `agents.<name>.authentication: none`. Authenticated provider execution inside OCI is future work, separate from the working native Codex path. Host credential stores are never mounted or copied into these workers.

OCI workers have a read-only image, no host mounts, no network, dropped capabilities, temporary storage, and fixed CPU, memory, process, and wall-time limits. Runtime preflight checks effective cgroup v2 limits and the ability to pause workers. Changes return through bounded import after the container is frozen and destroyed. Agent executables resolve inside the image. The [qualification plan](docs/security-remediation.md#real-container-qualification) covers actual Docker/Podman and cross-platform lifecycle tests.

A failed isolated run never falls back to host execution. Other native harnesses and macOS/Windows require qualification before they can use the native backend.

`swarm doctor` and `swarm work <issue> --dry-run` report execution setup and blockers. These checks help prepare the worker environment before acquiring work.

## Boundaries that need operator care

- Git CAS coordinates trusted writers; it does not cryptographically attest arbitrary state payloads. A principal able to push `refs/heads/swarm-state/*` can replace authoritative state outside the CLI. Restrict authoritative refs and checkpoint publication to trusted orchestration identities, including ruleset bypass privileges, and audit direct pushes.
- `trusted-local` shares the operator's OS account. Temporary HOME/XDG directories and filtered environment variables reduce accidental credential inheritance, but workers can still access other files and processes available to that account. Use a disposable, least-privilege worker account when selecting this mode.
- Reviewers use a separate snapshot and import only bounded `REVIEW.md` output. In `trusted-local`, their sandbox uses `sandbox-exec` on macOS, `bwrap` on Linux, or configured OCI containers; Windows requires the container path. The selected reviewer boundary must be available. Reviewer network access is denied by default; `agents.<name>.review_network: true` permits unrestricted egress only in compatibility mode. Native reviewer resource enforcement and descendant termination still need platform-specific validation.
- Internal state, WorkUnit files, scaffold destinations, and artifact imports use root-bound filesystem operations. Internal symbolic links and unsafe file types are rejected, and writes use exclusive temporary files followed by atomic replacement. This protects coordinator file operations from path substitution; it does not turn a shared OS account into a separate identity.
- GitHub branch protection, repository permissions, and human PR review remain the merge authority. Review the exact submitted SHA before merging or releasing.
- The local backend is for offline development and tests. It does not replace GitHub authorization.
- Hosted Grok Bot and OpenAI Dot use their own account, connector, and computer permissions. SwarmOS does not provision those connections or convert their comments into plan approvals. Local entries named `grok` or `dot` must select the supported harness they launch.

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
gh attestation verify swarm_<version>_<os>_<arch>.tar.gz --repo supernovae/SwarmOS
```
