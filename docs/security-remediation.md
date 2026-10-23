# Security remediation tracker

This work follows the 15 leads in the source audit of commit `2afdb8017f62ed07953c7ba932587042f9d66a9a`. That audit could not run its required sandbox and did not confirm vulnerabilities or assign severity. The implementation branch is `security/audit-remediation`.

The changes below add protections and adversarial regression fixtures. Native Codex now completes an authenticated WorkUnit using the existing login. Claude, OpenCode 2, and OhMyPi now have isolated tool adapters. Their full authenticated workflows, hosted authorization settings, additional platforms, and authenticated OCI have separate qualification records.

## Architectural direction

The [harness-first execution design](ARCHITECTURE.md#design-decision-use-the-harnesss-security-model-first) governs remediation choices. The default isolated backend uses native Codex or supported harness tool delegation on Linux. Harnesses own login and tool selection; the selected native sandbox or Bubblewrap executor enforces tool isolation; SwarmOS checks actual restrictions and retains task scope, leases, approval gates, artifact acceptance, and publishing authority. OCI remains an explicit alternative with offline workers.

Adapter qualification records a supported version/configuration and exercises its tool paths. Trust in the harness and operating system is part of the architecture. The tracker separates defects, integration gaps, and optional hardening.

## Findings and implementation

| Audit fingerprint | Change and regression coverage | Remaining validation |
| --- | --- | --- |
| `agent-task-cli-host-authority` | Codex uses restricted permission profiles; Claude, OpenCode 2, and OhMyPi use restricted delegated tools. All receive detached task files. Real tests cover file/network boundaries, read-only roles, filtered credentials, and cancellation. OCI remains available; policy tests cover backend selection and forbidden fallback. | Qualify additional versions, platforms, and harnesses. MCP/plugins/hooks are disabled in the native Codex adapter; enabling them needs separate coverage. |
| `contract.workunit-file.symlink-escape` | External WorkUnit reads are bounded and root-bound. Tests reject file and parent-directory symlinks to dummy outside YAML. | Confirm supported-platform behavior and any deployment route from issue authors to operator filesystem content. |
| `coordination.filestore.path-symlinks` | State and lock files use pinned directories; randomized exclusive temporary files replace predictable writes. Tests cover state/lock symlinks, the old temporary filename, and concurrent reads/CAS. | Native Windows/macOS filesystem matrix. |
| `coordinator-token.git-argv-visible-to-agent` | Git authentication is removed from command arguments and diagnostics are redacted. Git subprocess hardening disables candidate hooks and external helpers. | Native tests protect dummy host credentials and omit publishing variables. Extend this coverage to other runtimes. Provider login stays with the harness; repository publishing stays with SwarmOS. |
| `fork.coordinator.accepts-forged-agent-review` | Contributor review artifacts cannot satisfy trusted agent gates. Approval checks bind provenance and content to current authoritative artifacts, reject concurrent replanning, and run again at submission. The local fork lifecycle test requires authenticated maintainer approval. | Automatic trusted reviewer scheduling for fork plans remains open; contributors currently need an authenticated maintainer decision before proceeding through a gate. |
| `fork.gateway.comment-history-poll-amplification` | Polling has an overall deadline, bounded history, response matching, and backoff. Tests cover cancellation, edited/mismatched responses, and malformed data. | Large-history and delayed-response behavior against an owner-controlled staging repository, if needed. |
| `repository-scaffold.symlink-write-escape` | Scaffold and `.gitignore` operations reject linked destinations and use atomic root-bound writes. Fixtures cover force and preserve-existing modes. | Native Windows/macOS filesystem matrix. |
| `review-output-import-toctou` | Review output uses bounded descriptor reads, regular-file and link-count checks, and atomic destination writes. Tests cover hostile output and destination replacement. | Linux Codex detached-child cancellation passes. Expand lifecycle coverage on macOS/Windows; managed OCI destroys the worker before import. |
| `reviewer-execution-resource-bounds` | Managed OCI workers specify resource caps and verify effective cgroup limits; snapshot, output, and wall-time budgets bound coordinator work. | Check cancellation and resource controls for each supported harness/runtime. Add deployment limits where needed for unattended or shared workers; native reviewer resource enforcement remains unverified. |
| `reviewer-snapshot-unbounded-copy` | Snapshot copy has file, count, aggregate-byte, depth, and duration limits, cancellation, and special-file checks. Tests cover budgets and link policy. | Platform-specific cleanup and concurrent input mutation scenarios. |
| `tracker.local-tracker.state-and-lock-symlink-escape` | Local tracker state and locks use pinned root operations and atomic replacement; invalid state errors are surfaced. Tests protect outside canaries and reject invalid state types. | Native Windows/macOS filesystem matrix. |
| `validation-runs-candidate-tests-on-host` | Isolated validation uses approved-base commands through the selected native Codex, Bubblewrap, or OCI executor. Reacquiring an uncheckpointed worktree requires the approved base and rejects ordinary dirty changes, while preserving generated instruction pointers. | Native validation runs in a disposable snapshot and discards changes; cancellation is tested on Linux. Extend platform coverage. `trusted-local` validation still uses host permissions. |
| `workflows.fork-comment-triggered-actions-cost` | Repository and generated workflows perform admission checks before expensive checkout/toolchain work and bound processing. | Inspect owner-visible usage and admission behavior. GitHub still allocates a runner for eligible comment events; YAML cannot eliminate that cost. |
| `workflows.release.tag-release-authorization` | Existing tag/ancestry checks and release-environment requirement remain. | Owner must inspect tag rules, required reviewers, bypass identities, and effective permissions. No release or test tag is created for validation. |
| `workspace.active-directory.symlink-write-escape` | Bundle and artifact paths use safe pinned operations. Validation checks active-directory structure before ephemeral exclusions; regressions protect outside directories and instruction content. | Cross-platform checkpoint-to-provision tests and recorded collaborator policy. |

## Remaining qualification work

1. **Codex lifecycle and platforms:** the authenticated Linux slice covers all roles, interactive planning, unattended work, enforced human review, validation, and a local Draft PR. Add hosted GitHub publication in a staging repository, session-expiration/renewal scenarios, and macOS/Windows runtime checks. Existing lease-loss and stale-publication regression tests remain in place.
2. **Multi-harness workflows:** complete the authenticated all-role and mixed-routing tests. Claude and OhMyPi have passing authenticated tool tasks; OpenCode 2 has passing real offline dispatch tests and an unresolved provider test. Retain harness login ownership and qualify additional tool surfaces individually.
3. **Optional OCI and enterprise integration:** follow the [concrete roadmap](../todo.md#oci-authentication-and-enterprise-integrations) for provider-supported OCI authentication, credential separation/revocation, optional workload identity/OIDC, scoped GitHub Apps, and real-container cross-platform tests.
4. **Repository administration:** verify state-ref permissions, branch/tag rules, release reviewers, and bypass identities in the deployed GitHub repository. Repeat dependency/advisory checks as releases change.

Onboarding is install a supported harness and its runtime prerequisites, sign in through that harness, run `swarm repo doctor`, and start an approved WorkUnit. The [qualification record](native-qualification.md) lists the evidence for each adapter. OCI authentication is independent follow-up work.

## Validation record

The [final native qualification record](native-qualification.md) covers the follow-up to `d451180`, including fresh boundary tests and the private GitHub dogfood fixture. This review fixes ignored worker timeouts, canceled interactive reads consuming later input, incomplete plan output imports, and selected-role failures hidden by `doctor`. It also checks loopback reachability before testing sandbox denial and records the actual qualified harness version in provenance.

The Linux suite runs inside Bubblewrap with no external network, an allowlisted environment, read-only source/toolchain/module cache, bounded scratch storage, and explicit process, CPU, memory, file-size, and wall-time limits. Race tests use a cgroup memory limit because Go's race detector reserves a large virtual address space. The real OCI integration tests are opt-in and are skipped in this sandbox, which exposes no container-engine socket.

These are test-runner controls. Product integrations use the supported harness/runtime controls required by repository policy, following the architecture above.

| Check | Result |
| --- | --- |
| `gofmt -s`, `git diff --check` | Passed. |
| `go vet ./...` | Passed in the offline sandbox. |
| `go test -race ./...` | Passed on Linux/amd64, including adversarial fixtures; real OCI tests skipped without explicit opt-in. Limits: 3 GiB memory, zero swap, two CPUs, 128 processes, 1.5 GiB scratch, 256 MiB per file, and a four-minute wall deadline. |
| Native Codex 0.163.0, Linux/amd64 | Real sandbox boundary/lifecycle tests and the authenticated WorkUnit passed using the existing ChatGPT login. |
| macOS/arm64 test-package compilation | Passed; no native execution. |
| Windows/amd64 security-package compilation | Passed for `safefs`, `sandbox`, `agent`, and `validation`. Full CLI compilation was blocked by uncached `github.com/inconshreveable/mousetrap` v1.1.0; dependencies were not fetched by builds. |
| CLI and documentation checks | `work --help` and `doctor --help` verified; changed documentation links and Bash snippets passed; both admission scripts parsed as Python and Bash. |

Native macOS/Windows execution, real OCI resource/lifecycle enforcement, provider session-expiration scenarios, and hosted authorization settings remain unverified. File imports use atomic replacement per file; importing an entire change set is not transactional if a later filesystem operation fails. Empty-directory changes and symbolic-link outputs are not supported by the isolated importer.

The offline dependency scan used `govulncheck` v1.9.0 and Go 1.26.10 against `./...` on Linux/amd64. It reported **No vulnerabilities found** against the [Go vulnerability database snapshot](https://go.dev/doc/security/vuln/database) downloaded on October 22, 2026, with database modification time `2026-10-21T00:27:29Z`. The downloaded archive's SHA-256 is `895dc7fec6018000b2c4f999ee484339dfeba97b3dfe36bcd54720d101b7b1d8`. Platform-specific dependency paths and workflow advisories still need their own checks.

## Native Codex qualification

The Codex adapter requires at least 0.163.0 on Linux, supported execution flags, and passing runtime canaries. The real sandbox tests exercise outside credential canaries, symlinks, write protection, loopback denial, environment filtering, read-only roles, detached-child cancellation, missing login diagnostics, and validation that leaves source unchanged:

```bash
SWARM_TEST_CODEX_BINARY="$(command -v codex)" go test ./internal/sandbox -run '^TestNativeCodex(Boundary|ReadOnlyAndCancellation)$' -count=1 -v
```

The authenticated tests use the operator's existing Codex login and model quota. They use temporary fixtures and local coordination, including a local Draft PR:

```bash
codex login status
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/sandbox -run '^TestNativeCodexAuthenticatedFileTools$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/cli -run '^TestNativeCodexAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
```

The WorkUnit exercises interactive planning, a Codex reviewer, the human gate, unattended development/testing, native validation, and artifact-backed submission. Hosted GitHub publication remains covered by the existing tracker/fork regression fixtures and needs a separate staging run.

## Real-container qualification

Use a disposable audit machine or VM with a local Docker-compatible daemon and a preloaded Linux image pinned by digest. The image must provide standard `/bin/sh`, `sleep`, `cat`, `chmod`, `mkdir`, `rm`, `ln`, and `dd` tools. Set `SWARM_TEST_WORKER_IMAGE` to that image and `SWARM_TEST_WORKER_RUNTIME` to `docker` or `podman`, then run:

```bash
: "${SWARM_TEST_WORKER_IMAGE:?Set a preloaded image pinned by SHA-256 digest}"
: "${SWARM_TEST_WORKER_RUNTIME:?Select docker or podman}"
go test ./internal/sandbox -run '^TestWorkerOCI' -count=1
```

The fixtures check host-file and credential canaries, source protection, network interfaces and sockets, unsafe output archives, background-process cleanup, interactive stdin, deadline enforcement, and temporary-storage exhaustion. Complete the same matrix on Linux, macOS, and Windows, then record the exact runtime/image versions and results. Interactive stdin coverage must be supplemented with terminal behavior checks for each supported agent; provider authentication remains a separate adapter qualification.

See [SECURITY.md](../SECURITY.md) for the execution modes and operational controls. The previously rejected `project-plan.scan-ready-bulk-approval` lead remains closed unless new evidence warrants reopening it.
