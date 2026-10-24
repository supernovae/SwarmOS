# Harness qualification

Before SwarmOS raises a harness's minimum version or turns on a new tool capability, the harness is qualified: its boundary tests and an authenticated task must pass on each supported platform. This guide shows the current status and the commands to reproduce it. For setup and day-to-day behavior, see the [harness guides](harness/README.md).

## Current status

Native isolated execution is qualified on Linux/amd64, including WSL2. The sandbox package compiles for macOS/arm64 and Windows/amd64, but native runtime qualification on those platforms is still open.

| Harness | Minimum | Status |
| --- | --- | --- |
| [Codex](harness/codex.md) | 0.163.0 | Boundary tests and an authenticated all-role WorkUnit pass: interactive planning, a human gate, unattended development and testing, native validation, and submission. |
| [Claude Code](harness/claude.md) | 2.1.308 | Boundary tests and an authenticated interactive tool task pass. Full authenticated WorkUnit pending. |
| [OhMyPi](harness/omp.md) | 18.12.5 | RPC tool registration, boundary tests, and an authenticated interactive tool task pass. Full authenticated WorkUnit pending. |
| [OpenCode 2](harness/opencode.md) | 2.1.2 | Offline model and tool cycle, forged-call rejection, and cancellation pass. Live provider run pending. |
| Mixed routing | — | The opt-in test (Claude planning, Codex review, OpenCode development, OhMyPi testing) is implemented and waiting on four authenticated sessions. |

Newer stable releases run when they pass the interface and runtime checks. Artifact provenance records the actual version used.

## Codex

The boundary tests use dummy credential canaries and need no login:

```bash
SWARM_TEST_CODEX_BINARY="$(command -v codex)" go test ./internal/sandbox -run '^TestNativeCodex(Boundary|ReadOnlyAndCancellation)$' -count=1 -v
```

They cover outside credential canaries, symlinks, write protection, loopback denial, environment filtering, read-only roles, detached-child cancellation, missing-login diagnostics, and validation that leaves source unchanged.

The authenticated tests use your existing Codex login and model quota. They run against temporary local fixtures and publish nothing to GitHub:

```bash
codex login status
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/sandbox -run '^TestNativeCodexAuthenticatedFileTools$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/cli -run '^TestNativeCodexAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
```

### Adding a Codex release

Codex's [permission profiles are still beta](https://learn.chatgpt.com/docs/permissions).

1. Review permission, configuration-precedence, and tool changes in the new release.
2. Run the boundary and authenticated tests above on each intended platform, including interactive input and detached-process cancellation.
3. Update the minimum in `internal/sandbox/native.go` and the status table above.

Missing interfaces or failed canaries stop execution, and there is no flag to skip them. Projects can pin a specific installed binary with `agents.<name>.binary`.

## Claude Code, OpenCode 2, and OhMyPi

The offline interface tests use the installed harnesses and a synthetic local provider. They need no provider accounts:

```bash
SWARM_TEST_EXECUTOR=1 SWARM_TEST_OMP_BINARY="$(command -v omp)" \
  SWARM_TEST_OPENCODE_BINARY="$(command -v opencode)" \
  go test ./internal/sandbox -run 'Test(ToolExecutorBoundary|MCPToolCancellation|OMPRealToolRegistration|OpenCodeRealTools)$' -count=1 -v
```

For an authenticated run, use an existing login and pick the harness. The harness chooses the model unless `SWARM_TEST_HARNESS_MODEL` is set:

```bash
SWARM_TEST_AUTHENTICATED_HARNESS=claude \
  go test ./internal/sandbox -run '^TestAuthenticatedDelegatedTools$' -count=1 -v
SWARM_TEST_WORKFLOW_HARNESS=claude \
  go test ./internal/cli -run '^TestDelegatedAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
```

Both commands also accept `omp` or `opencode`.

## Mixed routing

```bash
SWARM_TEST_MIXED_AUTHENTICATED=1 \
  go test ./internal/cli -run '^TestMixedHarnessAuthenticatedWorkUnit$' -count=1 -v -timeout 15m
```

Optional `SWARM_TEST_CLAUDE_MODEL`, `SWARM_TEST_CODEX_MODEL`, `SWARM_TEST_OPENCODE_MODEL`, and `SWARM_TEST_OMP_MODEL` select models through the fixture's project configuration. The matching `_BINARY` variables select executables outside `PATH`. These tests use model quota and disposable local repositories.

Shell and file tools work across all adapters. Native subagents, hooks, plugins, external MCP services, browser control, and extra network tools each need their own boundary tests before they are enabled. See the [capability table](ARCHITECTURE.md#tool-capabilities).

## Sessions, cancellation, and recovery

Each harness handles its own login storage and token renewal. `doctor` reports a login through the harness's status command where one exists, but it does not make a model request. When a harness asks you to sign in again, do so, rerun `swarm repo doctor`, and resume with `swarm issue work <issue-number>`. Completed checkpoints remain, and an interrupted turn starts again from the task files.

Missing-login behavior, failed-run discard, deadlines, and cancellation are tested. Live session expiration and reauthentication mid-WorkUnit still need a provider-supported test session. Do not edit credential files or sign out a shared account to simulate expiration.

Artifact imports replace files atomically one at a time. All read-only role outputs are checked before anything is replaced, and developer imports check for concurrent workspace changes. A disk error during a multi-file import can still leave part of a change set applied, though acceptance and publication must still succeed separately. Whole-change-set rollback is planned hardening.
