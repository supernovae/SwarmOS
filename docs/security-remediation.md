# Security remediation tracker

This work follows the 15 leads in the source audit of commit `2afdb8017f62ed07953c7ba932587042f9d66a9a`. That audit could not run its required sandbox and did not confirm vulnerabilities or assign severity. The implementation branch is `security/audit-remediation`.

The changes below add protections and adversarial regression fixtures. A regression that rejects a synthetic attack demonstrates that specific control; it does not establish that every prerequisite of the original deployment scenario existed. Hosted settings, provider authentication, and the complete platform matrix still need validation.

## Architectural direction

The [harness-first execution design](ARCHITECTURE.md#design-decision-use-the-harnesss-security-model-first) governs remediation choices. Use the agent's supported login, permission, and sandbox mechanisms first. SwarmOS keeps ownership of task scope, leases, approval gates, accepted artifacts, and publishing authority. Security review helps verify those boundaries and identify specific gaps.

The OCI worker in this PR is one available implementation, not a universal requirement. Its offline-only behavior leaves normal provider-backed work unfinished. Prioritize a complete Codex workflow using the user's normal login and supported harness controls; add an external executor only where those controls cannot meet project policy. No generic credential broker, token database, or model proxy is planned.

Installing a harness or passing a permission flag is not enough to claim isolated support. Record the supported configuration and exercise the relevant tool paths. Ordinary use of a trusted harness is an architectural choice, not by itself a vulnerability. Keep confirmed defects, integration gaps, and optional hardening distinct when closing these leads.

## Findings and implementation

| Audit fingerprint | Change and regression coverage | Remaining validation |
| --- | --- | --- |
| `agent-task-cli-host-authority` | The current OCI path separates workers from the coordinator; policy tests cover explicit mode selection and forbidden fallback. | Qualify harness-native permissions and isolation with normal login, starting with Codex. Cover enabled file, shell, MCP/plugin, and subprocess paths; use an external executor for any required capability the harness lacks. |
| `contract.workunit-file.symlink-escape` | External WorkUnit reads are bounded and root-bound. Tests reject file and parent-directory symlinks to dummy outside YAML. | Confirm supported-platform behavior and any deployment route from issue authors to operator filesystem content. |
| `coordination.filestore.path-symlinks` | State and lock files use pinned directories; randomized exclusive temporary files replace predictable writes. Tests cover state/lock symlinks, the old temporary filename, and concurrent reads/CAS. | Native Windows/macOS filesystem matrix. |
| `coordinator-token.git-argv-visible-to-agent` | Git authentication is removed from command arguments and diagnostics are redacted. Git subprocess hardening disables candidate hooks and external helpers. | Verify that the chosen harness/executor boundary prevents agent tools from reading coordinator credentials. Provider login stays with the harness; repository publishing stays with SwarmOS. |
| `fork.coordinator.accepts-forged-agent-review` | Contributor review artifacts cannot satisfy trusted agent gates. Approval checks bind provenance and content to current authoritative artifacts, reject concurrent replanning, and run again at submission. The local fork lifecycle test requires authenticated maintainer approval. | Automatic trusted reviewer scheduling for fork plans remains open; contributors currently need an authenticated maintainer decision before proceeding through a gate. |
| `fork.gateway.comment-history-poll-amplification` | Polling has an overall deadline, bounded history, response matching, and backoff. Tests cover cancellation, edited/mismatched responses, and malformed data. | Large-history and delayed-response behavior against an owner-controlled staging repository, if needed. |
| `repository-scaffold.symlink-write-escape` | Scaffold and `.gitignore` operations reject linked destinations and use atomic root-bound writes. Fixtures cover force and preserve-existing modes. | Native Windows/macOS filesystem matrix. |
| `review-output-import-toctou` | Review output uses bounded descriptor reads, regular-file and link-count checks, and atomic destination writes. Tests cover hostile output and destination replacement. | Surviving-descendant and runtime termination tests on macOS, Linux, and Windows; managed OCI destroys the worker before import. |
| `reviewer-execution-resource-bounds` | Managed OCI workers specify resource caps and verify effective cgroup limits; snapshot, output, and wall-time budgets bound coordinator work. | Check cancellation and resource controls for each supported harness/runtime. Add deployment limits where needed for unattended or shared workers; native reviewer resource enforcement remains unverified. |
| `reviewer-snapshot-unbounded-copy` | Snapshot copy has file, count, aggregate-byte, depth, and duration limits, cancellation, and special-file checks. Tests cover budgets and link policy. | Platform-specific cleanup and concurrent input mutation scenarios. |
| `tracker.local-tracker.state-and-lock-symlink-escape` | Local tracker state and locks use pinned root operations and atomic replacement; invalid state errors are surfaced. Tests protect outside canaries and reject invalid state types. | Native Windows/macOS filesystem matrix. |
| `validation-runs-candidate-tests-on-host` | Current isolated validation uses approved-base commands inside OCI workers. Reacquiring an uncheckpointed worktree requires the approved base and rejects ordinary dirty changes, while preserving generated instruction pointers. | Route validation through a supported harness executor where it provides the required boundary, or retain a restricted external executor. Verify child-process cleanup and handoffs. `trusted-local` validation still uses host permissions. |
| `workflows.fork-comment-triggered-actions-cost` | Repository and generated workflows perform admission checks before expensive checkout/toolchain work and bound processing. | Inspect owner-visible usage and admission behavior. GitHub still allocates a runner for eligible comment events; YAML cannot eliminate that cost. |
| `workflows.release.tag-release-authorization` | Existing tag/ancestry checks and release-environment requirement remain. | Owner must inspect tag rules, required reviewers, bypass identities, and effective permissions. No release or test tag is created for validation. |
| `workspace.active-directory.symlink-write-escape` | Bundle and artifact paths use safe pinned operations. Validation checks active-directory structure before ephemeral exclusions; regressions protect outside directories and instruction content. | Cross-platform checkpoint-to-provision tests and recorded collaborator policy. |

## Remaining completion work

1. **Codex first:** integrate normal harness-owned login with supported native sandbox and permission controls. Use documented tool delegation only for gaps that need an external executor. Deliver planning, review, implementation, validation, and an authorized Draft PR for both interactive and unattended work. Keep provider session renewal in Codex and verify cancellation, lease loss, and resumption. An offline-only workflow does not complete this step.
2. **Small adapter contract:** record supported versions/platforms, login method, effective workspace/network permissions, enabled tool paths, and cleanup behavior. Check those capabilities in `swarm doctor` and preflight. Explain unsupported combinations and the simplest available setup; never silently fall back to `trusted-local`. Qualify Claude, OpenCode, and OhMyPi incrementally against the same requirements, using their own supported integrations.
3. **Hosted authorization:** have an owner inspect protections on authoritative Git-CAS refs, contributor-state refs, checkpoint publication, release tags, and the release environment, including bypass identities. Direct state-ref write authority is a separate deployment risk. Record settings that cannot be verified as open items.
4. **Dependency coverage:** repeat the Go vulnerability scan as dependencies and advisories change, and complete workflow dependency advisory review. The offline scan recorded below closes the earlier Go scan blocker for this source snapshot and platform.
5. **Focused verification:** test the selected harness/runtime on Linux, macOS, and Windows as support is added. Exercise workspace escapes, credential access, unapproved tool/network paths, cancellation, forged artifacts, and lease loss using dummy data. Run real OCI tests for the container path. Record demonstrated protections and open platform gaps. Preserve the independent filesystem, provenance, and approval fixes while harness integration is completed.

The intended onboarding is install, sign in through the harness, run `swarm doctor`, and start a WorkUnit. The current branch still requires an offline OCI setup or explicit `trusted-local` execution for launched agents. This is an open integration blocker, not the final onboarding design. Existing-session staging remains available, but does not complete the normal-login isolated workflow.

## Validation record

The Linux suite runs inside Bubblewrap with no external network, an allowlisted environment, read-only source/toolchain/module cache, bounded scratch storage, and explicit process, CPU, memory, file-size, and wall-time limits. Race tests use a cgroup memory limit because Go's race detector reserves a large virtual address space. The real OCI integration tests are opt-in and are skipped in this sandbox, which exposes no container-engine socket.

These are test-runner controls. Product integrations use the supported harness/runtime controls required by repository policy, following the architecture above.

| Check | Result |
| --- | --- |
| `gofmt -s`, `git diff --check` | Passed. |
| `go vet ./...` | Passed in the offline sandbox. |
| `go test -race -p 2 ./...` | Passed on Linux/amd64, including adversarial fixtures; real OCI tests skipped without explicit opt-in. Limits: 3 GiB memory, zero swap, two CPUs, 128 processes, 1.5 GiB scratch, 256 MiB per file, and a four-minute wall deadline. |
| macOS/arm64 test-package compilation | Passed; no native execution. |
| Windows/amd64 security-package compilation | Passed for `safefs`, `sandbox`, `agent`, and `validation`. Full CLI compilation was blocked by uncached `github.com/inconshreveable/mousetrap` v1.1.0; dependencies were not fetched by builds. |
| CLI and documentation checks | `work --help` and `doctor --help` verified; changed documentation links and Bash snippets passed; both admission scripts parsed as Python and Bash. |

Native macOS/Windows execution, real OCI resource/lifecycle enforcement, provider login reuse, and hosted authorization settings remain unverified. File imports use atomic replacement per file; importing an entire change set is not transactional if a later filesystem operation fails. Empty-directory changes and symbolic-link outputs are not supported by the isolated importer.

The offline dependency scan used `govulncheck` v1.9.0 and Go 1.26.10 against `./...` on Linux/amd64. It reported **No vulnerabilities found** against the [Go vulnerability database snapshot](https://go.dev/doc/security/vuln/database) downloaded on October 22, 2026, with database modification time `2026-10-21T00:27:29Z`. The downloaded archive's SHA-256 is `895dc7fec6018000b2c4f999ee484339dfeba97b3dfe36bcd54720d101b7b1d8`. Platform-specific dependency paths and workflow advisories still need their own checks.

## Running the real worker boundary tests

Use a disposable audit machine or VM with a local Docker-compatible daemon and a preloaded Linux image pinned by digest. The image must provide standard `/bin/sh`, `sleep`, `cat`, `chmod`, `mkdir`, `rm`, `ln`, and `dd` tools. Set `SWARM_TEST_WORKER_IMAGE` to that image and `SWARM_TEST_WORKER_RUNTIME` to `docker` or `podman`, then run:

```bash
: "${SWARM_TEST_WORKER_IMAGE:?Set a preloaded image pinned by SHA-256 digest}"
: "${SWARM_TEST_WORKER_RUNTIME:?Select docker or podman}"
go test ./internal/sandbox -run '^TestWorkerOCI' -count=1
```

The fixtures check host-file and credential canaries, source protection, network interfaces and sockets, unsafe output archives, background-process cleanup, interactive stdin, deadline enforcement, and temporary-storage exhaustion. Complete the same matrix on Linux, macOS, and Windows, then record the exact runtime/image versions and results. Interactive stdin coverage must be supplemented with terminal behavior checks for each supported agent; provider authentication remains a separate adapter qualification.

See [SECURITY.md](../SECURITY.md) for the execution modes and operational controls. The previously rejected `project-plan.scan-ready-bulk-approval` lead remains closed unless new evidence warrants reopening it.
