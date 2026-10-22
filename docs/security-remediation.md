# Security remediation tracker

This work follows the 15 leads in the source audit of commit `2afdb8017f62ed07953c7ba932587042f9d66a9a`. That audit could not run its required sandbox and did not confirm vulnerabilities or assign severity. The implementation branch is `security/audit-remediation`.

The changes below add protections and adversarial regression fixtures. A regression that rejects a synthetic attack demonstrates that specific control; it does not establish that every prerequisite of the original deployment scenario existed. Hosted settings, provider authentication, and the complete platform matrix still need validation.

## Findings and implementation

| Audit fingerprint | Change and regression coverage | Remaining validation |
| --- | --- | --- |
| `agent-task-cli-host-authority` | Default isolated mode separates workers from the coordinator; explicit compatibility mode and approved isolation requirements are enforced. Policy tests cover blocked credentials and forbidden fallback. | Real runtime canary tests for all roles, interactive/unattended runs, and safe provider authentication on all platforms. |
| `contract.workunit-file.symlink-escape` | External WorkUnit reads are bounded and root-bound. Tests reject file and parent-directory symlinks to dummy outside YAML. | Confirm supported-platform behavior and any deployment route from issue authors to operator filesystem content. |
| `coordination.filestore.path-symlinks` | State and lock files use pinned directories; randomized exclusive temporary files replace predictable writes. Tests cover state/lock symlinks, the old temporary filename, and concurrent reads/CAS. | Native Windows/macOS filesystem matrix. |
| `coordinator-token.git-argv-visible-to-agent` | Git authentication is removed from command arguments and diagnostics are redacted. Git subprocess hardening disables candidate hooks and external helpers. | Credential scope and actual worker process isolation; environment transport alone does not protect a shared OS identity. |
| `fork.coordinator.accepts-forged-agent-review` | Contributor review artifacts cannot satisfy trusted agent gates. Approval checks bind provenance and content to current authoritative artifacts, reject concurrent replanning, and run again at submission. The local fork lifecycle test requires authenticated maintainer approval. | Automatic trusted reviewer scheduling for fork plans remains open; contributors currently need an authenticated maintainer decision before proceeding through a gate. |
| `fork.gateway.comment-history-poll-amplification` | Polling has an overall deadline, bounded history, response matching, and backoff. Tests cover cancellation, edited/mismatched responses, and malformed data. | Large-history and delayed-response behavior against an owner-controlled staging repository, if needed. |
| `repository-scaffold.symlink-write-escape` | Scaffold and `.gitignore` operations reject linked destinations and use atomic root-bound writes. Fixtures cover force and preserve-existing modes. | Native Windows/macOS filesystem matrix. |
| `review-output-import-toctou` | Review output uses bounded descriptor reads, regular-file and link-count checks, and atomic destination writes. Tests cover hostile output and destination replacement. | Surviving-descendant and runtime termination tests on macOS, Linux, and Windows; managed OCI destroys the worker before import. |
| `reviewer-execution-resource-bounds` | Managed OCI workers specify resource caps and verify effective cgroup limits; snapshot, output, and wall-time budgets bound coordinator work. | Real resource-exhaustion tests and cleanup evidence. Native compatibility reviewer CPU, memory, process, and storage enforcement remains incomplete. |
| `reviewer-snapshot-unbounded-copy` | Snapshot copy has file, count, aggregate-byte, depth, and duration limits, cancellation, and special-file checks. Tests cover budgets and link policy. | Platform-specific cleanup and concurrent input mutation scenarios. |
| `tracker.local-tracker.state-and-lock-symlink-escape` | Local tracker state and locks use pinned root operations and atomic replacement; invalid state errors are surfaced. Tests protect outside canaries and reject invalid state types. | Native Windows/macOS filesystem matrix. |
| `validation-runs-candidate-tests-on-host` | Isolated validation uses approved-base commands inside the worker boundary. Reacquiring an uncheckpointed worktree requires the approved base and rejects ordinary dirty changes, while preserving generated instruction pointers. | All-platform candidate execution tests and shared/separate-checkout handoff coverage. Explicit compatibility mode still runs validation on the host. |
| `workflows.fork-comment-triggered-actions-cost` | Repository and generated workflows perform admission checks before expensive checkout/toolchain work and bound processing. | Inspect owner-visible usage and admission behavior. GitHub still allocates a runner for eligible comment events; YAML cannot eliminate that cost. |
| `workflows.release.tag-release-authorization` | Existing tag/ancestry checks and release-environment requirement remain. | Owner must inspect tag rules, required reviewers, bypass identities, and effective permissions. No release or test tag is created for validation. |
| `workspace.active-directory.symlink-write-escape` | Bundle and artifact paths use safe pinned operations. Validation checks active-directory structure before ephemeral exclusions; regressions protect outside directories and instruction content. | Cross-platform checkpoint-to-provision tests and recorded collaborator policy. |

## Remaining completion work

1. **Provider authentication:** implement and verify provider-specific brokerage or narrowly scoped credentials for existing Claude, Codex, OpenCode, and OhMyPi authentication. Keep unsupported paths blocked and never mount unrestricted host credential stores. Test model access, expiry, cancellation, and denial of credential export or generic proxy use.
2. **Runtime matrix:** run Linux first, then macOS and Windows with their supported OCI/runtime setup. Cover planner, developer, tester, reviewer, and validation; interactive and unattended execution; filesystem/process/network canaries; quota enforcement; failed cleanup; artifact import; lease loss; approval and publication gates. Confirm that missing capabilities cannot cause host fallback.
3. **Hosted authorization:** have an owner inspect protections on authoritative Git-CAS refs, contributor-state refs, checkpoint publication, release tags, and the release environment, including bypass identities. Direct state-ref write authority is a separate deployment risk. Record settings that cannot be verified as open items.
4. **Dependency coverage:** repeat the Go vulnerability scan as dependencies and advisories change, and complete workflow dependency advisory review. The offline scan recorded below closes the earlier Go scan blocker for this source snapshot and platform.
5. **Evidence and setup:** retain sandbox test results, supported runtime/image combinations, and each lead's closure rationale. Existing dogfooding setups must configure an isolated runtime or explicitly select permitted `trusted-local` execution. Reapprove tasks whose approved policy digest no longer matches. Independent fixes can ship before provider adapters and the full platform matrix are complete.

## Validation record

The Linux suite runs inside Bubblewrap with no external network, an allowlisted environment, read-only source/toolchain/module cache, bounded scratch storage, and explicit process, CPU, memory, file-size, and wall-time limits. Race tests use a cgroup memory limit because Go's race detector reserves a large virtual address space. The real OCI integration tests are opt-in and are skipped in this sandbox, which exposes no container-engine socket.

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
