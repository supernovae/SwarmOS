# Harness qualification

Linux adapters are implemented for Codex, Claude Code, OpenCode 2, and OhMyPi. Codex retains its native sandbox; the other adapters use supported tool controls with Bubblewrap. All keep provider authentication with the harness. Full multi-harness qualification still needs the authenticated OpenCode and mixed-workflow runs below. OCI authentication and enterprise identity remain separate roadmap work.

## Evidence at this review

Reviewed PR #19 at `d451180` and the follow-up changes on `security/audit-remediation`, October 22, 2026.

| Area | Evidence and remaining work |
| --- | --- |
| Fresh independent checks | `go vet ./...` and the full Linux race suite passed. Real Codex sandbox tests cover outside files, symlinks, read-only roles, network denial, filtered environment, and detached-child cancellation. Bubblewrap tests cover the same tool boundary, actual file editing, and MCP request cancellation. |
| Codex authentication | The earlier `d451180` WorkUnit passed interactive planning, reviewer execution, a human gate, unattended development/testing, validation, and local Draft PR creation. Normal ChatGPT login remains available in the current `doctor` run. |
| Claude Code 2.1.308 | A real authenticated interactive tool task passed with the operator's normal Claude login: denied synthetic host-file read, workspace write/read, and shell verification. Full WorkUnit qualification remains to run. |
| OhMyPi 18.12.5 | The real RPC interface exposes exactly the approved task tools. An authenticated interactive tool task passed through its existing provider login, including synthetic host-file denial and workspace editing/testing. Full WorkUnit qualification remains to run. |
| OpenCode 2.1.2 | The installed CLI passes an offline model/tool cycle, rejects a forged host-shell call, and stops detached tool children on cancellation through its private server and isolated MCP tools. Live provider execution remains unqualified: the requested test model was unavailable and the harness default free-tier request was rejected. Further model calls were stopped at the operator's request. |
| Mixed routing | The opt-in test routes Claude planning → Codex review → OpenCode development → OhMyPi testing, with a human gate, validation, and local Draft PR. It is implemented but has not completed against four authenticated sessions. |
| Fixed implementation defects | OpenCode now waits for connected tools before its first model request and checks merged permissions. Trusted-local OpenCode uses V2 commands. MCP cancellation reaches tool subprocesses. Existing timeout, interactive cancellation, safe-import, role-policy, provenance and approval regressions remain covered. |
| Versions and platforms | Minimum stable versions: Codex 0.163.0, Claude 2.1.308, OpenCode 2.1.2, OhMyPi 18.12.5. Interface and runtime checks also apply to newer releases. Tests ran on Linux/amd64 under WSL2. The sandbox package compiles for macOS/arm64 and Windows/amd64; native runtime qualification remains open. |
| Hosted staging | The operator is continuing `supernovae/swarm-test` separately. No staging publication or remote mutation was performed during this adapter work. |
| Optional roadmap | Authenticated OCI, enterprise workload identity/OIDC, scoped GitHub Apps, and additional harness capabilities remain in the [roadmap](../todo.md#oci-authentication-and-enterprise-integrations). |

The fresh test runner had no external network or host credentials, read-only source/toolchain/binary mounts, and private scratch. Limits were two CPUs, 3 GiB memory, zero swap, 128 processes, 1.5 GiB scratch, 256 MiB per file, and four minutes wall time. The real native suite used only a private loopback namespace and dummy credential canaries.

The earlier remote-free dogfood fixture passed local WorkUnit creation, approval, and linting. CLI examples and documentation are checked separately from authenticated workflow qualification.

## Adding a Codex release

Codex's [permission profiles are still beta](https://learn.chatgpt.com/docs/permissions). The adapter accepts stable releases from 0.163.0 onward when the required execution flags and runtime boundary checks pass.

1. Review permission, configuration-precedence, and tool changes in the new release.
2. Run the boundary, authenticated file-tool, and WorkUnit tests below on each intended platform. Include interactive input and detached-process cancellation.
3. Record the version, OS/architecture, and results here. Missing interfaces or failed canaries stop execution; there is no verification-bypass flag. Projects may select a specific installed binary with `agents.<name>.binary`.

From the reviewed SwarmOS checkout on a Linux worker with Codex 0.163.0 and its normal login:

```bash
codex --version
codex login status
SWARM_TEST_CODEX_BINARY="$(command -v codex)" go test ./internal/sandbox -run '^TestNativeCodex(Boundary|ReadOnlyAndCancellation)$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/sandbox -run '^TestNativeCodexAuthenticatedFileTools$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/cli -run '^TestNativeCodexAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
go build -o /tmp/swarm-native-review ./cmd/swarm
```

The authenticated tests use model quota and temporary local fixtures. They leave provider credentials with Codex. When boundary checks fail, use a Linux worker where the sandbox and local socket setup are available; keep the same SwarmOS execution policy.

## Other harnesses and mixed routing

The offline interface tests use installed harnesses and a synthetic local provider. They do not need provider accounts:

```bash
SWARM_TEST_EXECUTOR=1 SWARM_TEST_OMP_BINARY="$(command -v omp)" \
  SWARM_TEST_OPENCODE_BINARY="$(command -v opencode)" \
  go test ./internal/sandbox -run 'Test(ToolExecutorBoundary|MCPToolCancellation|OMPRealToolRegistration|OpenCodeRealTools)$' -count=1 -v
```

For an authenticated task, use an existing login and choose the harness to test. Model selection stays with the harness unless `SWARM_TEST_HARNESS_MODEL` is set explicitly for this test:

```bash
SWARM_TEST_AUTHENTICATED_HARNESS=claude \
  go test ./internal/sandbox -run '^TestAuthenticatedDelegatedTools$' -count=1 -v
SWARM_TEST_WORKFLOW_HARNESS=claude \
  go test ./internal/cli -run '^TestDelegatedAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
SWARM_TEST_MIXED_AUTHENTICATED=1 \
  go test ./internal/cli -run '^TestMixedHarnessAuthenticatedWorkUnit$' -count=1 -v -timeout 15m
```

The first two commands also accept `omp` or `opencode`. For full workflow tests, optional `SWARM_TEST_CLAUDE_MODEL`, `SWARM_TEST_CODEX_MODEL`, `SWARM_TEST_OPENCODE_MODEL`, and `SWARM_TEST_OMP_MODEL` select models through the fixture's project configuration. Matching `_BINARY` variables select installed executables outside `PATH`. These commands use model quota and local disposable repositories, with no GitHub publication.

Safe file and shell tools are available across the adapters. Native subagents, hooks, plugins, external MCP services, browser control, and additional network tools need integration-specific boundary tests before enablement. See the [capability table](ARCHITECTURE.md#tool-capabilities).

## Hosted dogfood run

Use the private `supernovae/swarm-test` fixture. Supply a short-lived GitHub credential restricted to that repository through `SWARM_GITHUB_TOKEN`, with Contents, Issues, and Pull requests read/write permissions and Metadata read access. The fixture has no Actions workflows and needs no workflow or repository-administration permission. The account approving tasks must have the repository role required by SwarmOS. A separate low-privilege actor is needed to test GitHub's rejection of unauthorized approvals; using the owner's account cannot establish that result.

First clone the fixture and inspect the effective setup:

```bash
: "${SWARM_GITHUB_TOKEN:?Load the swarm-test-only credential through your local secret setup}"
export GH_TOKEN="$SWARM_GITHUB_TOKEN"
gh repo clone supernovae/swarm-test
cd swarm-test
SWARM=/tmp/swarm-native-review
dogfood() { "$SWARM" --backend github --repo supernovae/swarm-test "$@"; }
dogfood doctor
dogfood issue create --title 'Native Codex dogfood greeting' \
  --objective 'Create hello.txt containing exactly hello from SwarmOS followed by a newline.' \
  --constraint 'Keep repository policy unchanged. Make only this small text-file change.' \
  --acceptance 'The greeting required check passes.' --gate human
```

Set `ISSUE` to the number printed above. Approve the task, plan interactively, and check that implementation and submission wait for human review. Press Enter after the planner finishes to accept the phase.

```bash
ISSUE=1
expect_rejection() {
  if "$@"; then
    printf '%s\n' 'Unexpected acceptance; stop and inspect this run.' >&2
    return 1
  fi
}
dogfood issue approve "$ISSUE"
dogfood work "$ISSUE" --phase plan --headless=false
expect_rejection dogfood submit "$ISSUE"
dogfood review "$ISSUE"
expect_rejection dogfood work "$ISSUE" --phase implement
dogfood status "$ISSUE" --json
git ls-remote origin "refs/heads/swarm-state/issue-$ISSUE"
```

Inspect `.swarm/worktrees/issue-$ISSUE/.swarm/active/PLAN.md` and `TEST_PLAN.md`, then approve the plan and run development, testing, and validation unattended:

```bash
dogfood review "$ISSUE" --approve --notes 'Reviewed the bounded greeting plan and test plan'
dogfood work "$ISSUE" --headless
dogfood validate "$ISSUE"
dogfood submit "$ISSUE"
dogfood status "$ISSUE" --json
gh pr list --repo supernovae/swarm-test --state open --json number,isDraft,headRefName,url
```

Record the claim ID, base/head SHAs, artifact provenance, state ref, and Draft PR URL. Keep the Draft PR open for review. Stop on unexpected acceptance or failure; inspect the error before continuing.

For the adversarial pass, use a fresh WorkUnit in this same staging repository. Check competing claims from a second clone, stale submissions after release/reclaim, forged approval comments, altered plan contents after approval, and protected-policy edits in the task worktree. Compare state refs and artifact digests before and after each attempt. Expected results are claim fencing, an unsatisfied gate, or protected-path rejection, with no unauthorized Draft PR. Use a repository-restricted low-privilege identity for authorization denials. The local race suite already covers these invariants; hosted results remain to be recorded. GitHub rules and collaborator permissions control direct Git writes by users outside SwarmOS.

## Sessions, cancellation, and recovery

Codex handles [login storage and automatic token renewal](https://learn.chatgpt.com/docs/auth). `doctor` reports an existing login from `codex login status`; it does not make a model request to check whether the provider will accept or renew that session. When Codex asks for reauthentication, use `codex login` or its supported `codex login --device-auth`, then rerun `swarm repo doctor` and resume the WorkUnit with `swarm issue work <issue-number>`. Completed checkpoints remain available; an interrupted ephemeral model turn starts again from task files.

Missing-login behavior, failed-run discard, deadlines, and cancellation are tested. Live session expiration, renewal failure, and reauthentication during a WorkUnit still need a provider-supported test account/session. Do not edit credential files or sign out a shared operator account to simulate expiration. Record that scenario separately when a suitable session is available.

Artifacts are replaced atomically per file. All read-only role outputs are checked before any replacement, and developer imports check for concurrent workspace changes. A disk or filesystem error during a multi-file import can still leave part of the local change set applied; acceptance and publication must succeed separately. Whole-change-set rollback is follow-up hardening. Codex has bounded imports and phase deadlines. Delegated tools also have per-process resource limits and private temporary-storage limits; neither path currently supplies aggregate task memory or workspace-disk quotas. Shared unattended deployments can add OS service limits; authenticated OCI resource isolation remains optional roadmap work.
