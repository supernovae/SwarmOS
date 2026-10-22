# Native Codex qualification

The native Codex workflow is ready for focused Linux dogfooding. The remaining release checks are an authenticated rerun of these fixes, hosted coordination and publication, and provider session recovery. Native authentication and isolation remain the default; OCI authentication and enterprise identity are separate roadmap work.

## Evidence at this review

Reviewed PR #19 at `d451180` and the follow-up changes on `security/audit-remediation`, October 22, 2026.

| Area | Evidence and remaining work |
| --- | --- |
| Fresh independent checks | Linux/amd64 `go vet` and the full race suite passed. Real Codex 0.163.0 sandbox tests passed inside a credential-free Bubblewrap environment: outside-file and symlink denial, allowed writes, read-only roles, loopback denial, filtered credential variables, detached-child cancellation, missing-login diagnostics, and disposable validation. |
| Existing authenticated evidence | At `d451180`, the ChatGPT-authenticated WorkUnit passed interactive planning, reviewer execution, the human gate, unattended development/testing, native validation, and local Draft PR creation. The authenticated file-tool test exercised `apply_patch` and shell attempts against dummy outside credentials. These authenticated tests have not been rerun for this follow-up. |
| Fixed implementation defects | Native workers now honor shorter requested timeouts. Canceling interactive input leaves no blocked reader to consume a resumed session's input. Invalid or missing plan outputs are rejected before replacing existing artifacts. `doctor` checks selected roles even when their agent's allowed-role list excludes them, and selected failures remain failures in both modes. Regression tests cover these cases. |
| Verification improvements | The loopback probe first confirms coordinator-side reachability. Diagnostics distinguish installed binaries, selected role failures, and optional harnesses. The version allowlist retains exact qualification and records the actual verified version in provenance. |
| Environment qualification | Fresh native tests ran on Linux/amd64 under WSL2. This session's authenticated runner blocks loopback sockets and access to Codex's required app-server socket directory. No macOS runner was available. The updated sandbox package compiled for macOS/arm64 and Windows/amd64; runtime qualification remains open. Other Linux architectures need their own recorded runs. |
| Hosted staging | The private `supernovae/swarm-test` repository contains the approved 11-file fixture at `30b07d1`. Claims, state refs, gates, artifact submission, and hosted Draft PR creation await the operator's local run below, using repository-scoped credentials. |
| Optional roadmap | Authenticated OCI, enterprise workload identity/OIDC, scoped GitHub Apps, and Claude/OpenCode/OhMyPi isolation adapters remain in the [roadmap](../todo.md#oci-authentication-and-enterprise-integrations). |

The fresh test runner had no external network or host credentials, read-only source/toolchain/binary mounts, and private scratch. Limits were two CPUs, 3 GiB memory, zero swap, 128 processes, 1.5 GiB scratch, 256 MiB per file, and four minutes wall time. The real native suite used only a private loopback namespace and dummy credential canaries.

CLI help, documentation links/anchors, and 17 shell blocks passed their checks. A remote-free scratch copy of the dogfood fixture successfully created, approved, and linted a local WorkUnit with the human gate.

## Adding a Codex release

Keep exact version qualification: Codex's [permission profiles are still beta](https://learn.chatgpt.com/docs/permissions), and their configuration precedence and tool surfaces can change.

1. In a qualification branch, add the candidate's exact version to `nativeCodexVersions` in `internal/sandbox/native.go`, preserving earlier qualified versions. Review its permission and tool changes against `codexPolicy`.
2. Run the boundary, authenticated file-tool, and full WorkUnit tests below on each intended platform. Exercise interactive input and detached-process cancellation. Record the harness version, OS/architecture, and results.
3. Merge the allowlist addition with its evidence. Until then, repositories can set `agents.codex.binary` to an installed qualified release alongside the operator's newer CLI. There is no wildcard version or production verification bypass.

From the reviewed SwarmOS checkout on a Linux worker with Codex 0.163.0 and its normal login:

```bash
codex --version
codex login status
SWARM_TEST_CODEX_BINARY="$(command -v codex)" go test ./internal/sandbox -run '^TestNativeCodex(Boundary|ReadOnlyAndCancellation)$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/sandbox -run '^TestNativeCodexAuthenticatedFileTools$' -count=1 -v
SWARM_TEST_CODEX_AUTHENTICATED=1 go test ./internal/cli -run '^TestNativeCodexAuthenticatedWorkUnit$' -count=1 -v -timeout 10m
go build -o /tmp/swarm-native-review ./cmd/swarm
```

The authenticated tests use model quota and temporary local fixtures. They leave provider credentials with Codex. When this runner's boundary checks fail, use a Linux worker where Codex's native sandbox and local socket setup are available; keep the same SwarmOS execution policy.

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

Codex handles [login storage and automatic token renewal](https://learn.chatgpt.com/docs/auth). `doctor` reports an existing login from `codex login status`; it does not make a model request to check whether the provider will accept or renew that session. When Codex asks for reauthentication, use `codex login` or its supported `codex login --device-auth`, then rerun `swarm doctor` and resume the WorkUnit with `swarm work <issue-number>`. Completed checkpoints remain available; an interrupted ephemeral model turn starts again from task files.

Missing-login behavior, failed-run discard, deadlines, and cancellation are tested. Live session expiration, renewal failure, and reauthentication during a WorkUnit still need a provider-supported test account/session. Do not edit credential files or sign out a shared operator account to simulate expiration. Record that scenario separately when a suitable session is available.

Artifacts are replaced atomically per file. All read-only role outputs are checked before any replacement, and developer imports check for concurrent workspace changes. A disk or filesystem error during a multi-file import can still leave part of the local change set applied; acceptance and publication must succeed separately. Whole-change-set rollback is follow-up hardening. Native workers have bounded import sizes and phase deadlines, but no product-enforced CPU, memory, PID, or scratch-disk quotas. Shared unattended deployments can add OS service limits; authenticated OCI resource isolation remains optional roadmap work.
