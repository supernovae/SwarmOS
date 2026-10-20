# Security Policy

## Supported versions

The latest tagged release is supported according to its release notes. Always match your installed binary (`swarm version`) with the documentation for that release or commit.

## Reporting a vulnerability

Report vulnerabilities privately through [GitHub Security Advisories](https://github.com/supernovae/SwarmOS/security/advisories/new), not a public issue. Include the SwarmOS version or commit, operating system, backend, configuration, and a minimal reproduction where safe to share.

## Threat model and enforced boundaries

SwarmOS is a trusted-repository orchestration tool. It does not make arbitrary issue text, agent processes, shell validation commands, or a Git worktree safe by itself.

The enforced controls in the Git-CAS workflow are:

- A task is a draft until an authorized actor runs `swarm issue approve <issue>`, which persists the WorkUnit digest, policy digest, and trusted base SHA before marking it ready.
- State is read and written through authoritative Git CAS refs (or the explicitly selected locked local backend). Comments are audit projections and cannot create claims, approvals, or artifacts.
- Worker-owned claim, artifact, checkpoint, and submission mutations are fenced by a unique claim ID and check task/policy revision, expiry, and ownership. Authorized task approval and human review are independent CAS operations. A stale worker must stop rather than publish to a successor's task.
- Plan approval is bound to the current task, plan, and test-plan digests. Agent approval and human approval are distinct; a human gate cannot be satisfied by an agent verdict.
- Validation obtains checks and protected paths from the approved base, not the candidate worktree. A candidate cannot remove validation policy to pass.
- Configured agents are allowlisted by name, role, executable, and optional model/arguments. `mock` is allowed only for explicit local/test use.

HTML comments are stripped before parsing an issue contract. This reduces one hidden-text input path; it does **not** prevent prompt injection in visible issue text, source files, tool output, dependencies, or an agent's external context. Treat all untrusted content as data and review generated plans and changes.

## Important limitations

- Git CAS coordinates trusted writers; it does not cryptographically attest arbitrary state payloads. A principal able to push `refs/heads/swarm-state/*` can replace authoritative state outside the CLI. Restrict those refs and checkpoint branches to trusted orchestration operators, protect their credentials, and audit direct pushes. A compromised state writer is outside this boundary.
- A Git worktree is not a sandbox. It does not restrict host files, network, processes, or inherited credentials.
- Reviewers run in a separate immutable snapshot and may import only `REVIEW.md`. The current implementation uses `sandbox-exec` on macOS, `bwrap` on Linux, or a configured OCI container runtime; Windows has no native host reviewer sandbox and therefore requires the container path. An unavailable selected boundary fails closed. Linux `bwrap` requires host user namespaces. Containers are reviewer-only and use a read-only root, dropped capabilities/no-new-privileges, read-only snapshot, and explicit writable output/scratch mounts; Linux Docker requires accessible bind mounts. Reviewer network access is denied by default. Setting `agents.<name>.review_network: true` permits unrestricted host/container egress for that reviewer; it is not domain-filtered and should be used only when a cloud model CLI requires it.
- Agent children use a temporary credential-free scratch home rather than inherited host `HOME`, XDG, SSH, Git, or publishing-token state. Only exact configured `agents.<name>.env` entries are forwarded; configuration rejects `GH_*`, `GITHUB_*`, `GIT_*`, `SSH_*`, `HOME`, and `XDG_*` names. This does not isolate arbitrary non-reviewer code: planner, developer, and tester host processes share the user UID and can directly read host files or credential stores. Do not relabel secrets into permitted variables; keep publication credentials in the coordinator process.
- GitHub branch protection, repository permissions, and human PR review remain the merge authority. A Draft PR and validation artifact do not prove each acceptance criterion or authorize merging.
- The explicit local backend is for offline development and deterministic tests; it is not a GitHub authorization substitute.
- Hosted Grok Bot and OpenAI Dot use their own account, connector, and computer permissions. SwarmOS does not provision those connections or convert their GitHub comments into plan approvals. Local agent entries named `grok` or `dot` must select the supported harness they actually launch; the name alone does not connect a hosted service.

## Operational guidance

1. Commit strict `.swarm/config.yaml`, workflows, prompts, and validation policy to the default branch.
2. Use explicit GitHub backend configuration in production; use `--backend local` only when intentionally offline.
3. Create draft tasks, inspect them, then approve the exact revision. Reapprove after substantive issue or policy changes.
4. Use human plan gates for changes requiring maintainer judgment. Review `--dry-run` policy output before unattended work.
5. Use disposable/least-privilege agent environments; review sandbox configuration on every supported platform.
6. Require CI and branch protection for the exact submitted SHA before release or merge.

## Release administration

Release publication runs the exact tagged commit through the repository's reusable lint, vulnerability, and cross-platform test workflow and requires that release tag to be reachable from the default branch. Publishing uses the GitHub `release` environment. Administrators must configure required reviewers and protection for that environment and protect release tags/branches in GitHub; workflow YAML alone cannot enforce remote repository settings.

## Supply chain and release evidence

Release archives are built and attested in GitHub Actions after passing the lint, vulnerability, and multi-platform test matrix. You can verify the provenance of any release archive with the GitHub CLI:

```bash
gh attestation verify swarm_<version>_<os>_<arch>.tar.gz --repo supernovae/SwarmOS
```
