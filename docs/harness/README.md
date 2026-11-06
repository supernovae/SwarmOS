# Agent Harness Guides

SwarmOS works with the coding agents you already use. You install a harness, sign in through its normal login, and point SwarmOS at it. The harness keeps your provider session and model connection. SwarmOS handles the WorkUnit contract, the claim on the task, the execution boundary, and publishing the result.

Each guide covers one harness: how to set it up, how SwarmOS runs it, what its tools can and cannot reach, and how to fix common setup problems.

| Harness | Driver | Minimum version | Isolated boundary on Linux | Guide |
| --- | --- | --- | --- | --- |
| OpenAI Codex | `codex` | 0.163.0 | Codex's native permission profile and sandbox | [codex.md](codex.md) |
| Claude Code | `claude` | 2.1.308 | Restricted mode with SwarmOS tools in Bubblewrap | [claude.md](claude.md) |
| OpenCode 2 | `opencode` | 2.1.2 | Private server with SwarmOS tools in Bubblewrap | [opencode.md](opencode.md) |
| OhMyPi | `omp` | 18.12.5 | RPC host tools in Bubblewrap | [omp.md](omp.md) |

Newer stable releases are accepted when they still provide the required interface and pass the runtime boundary checks. Pre-release versions are rejected.

> [!TIP]
> Coordinating from a hosted assistant? [Grok Bot](../grokbot.md) and [OpenAI Dot](../openai-dot.md) have their own guides. They drive `swarm` and route work to the harnesses on this page.

## Quick setup

1. Install the harness and sign in with its normal login flow.
2. For Claude Code, OpenCode 2, or OhMyPi, install Bubblewrap (`bwrap`), Python 3, Bash, and `prlimit` under `/usr`. Codex does not need these.
3. Add or check the agent entry in `.swarm/config.yaml`. `swarm repo init` adds an entry for each built-in harness:

   ```yaml
   execution:
     mode: isolated
     backend: native
   agents:
     codex:
       binary: codex
     claude:
       binary: claude
       model: your-preferred-model   # optional; the harness default is used otherwise
   ```

4. Run `swarm repo doctor`. It checks the selected agents' versions, required interfaces, login status where the harness reports it, and the actual filesystem and network restrictions.
5. Preview the task with `swarm issue work <issue> --dry-run`, then run it.

## Agent configuration

The key under `agents:` is the policy and audit name that appears in provenance. `harness` selects the driver and defaults to the key, so an entry named `reviewer-claude` needs `harness: claude`.

| Field | Purpose |
| --- | --- |
| `binary` | Executable name or path. Use a path to pin a specific installed release. |
| `harness` | `codex`, `claude`, `opencode`, or `omp`. Defaults to the entry name. |
| `model` | Passed to the harness exactly as written. Leave it unset to use the harness default. |
| `roles` | Restrict the entry to specific roles (`planner`, `reviewer`, `developer`, `tester`). |
| `args` | Extra leading arguments for `trusted-local` and OCI launches. Native isolated runs use SwarmOS's fixed harness settings. |
| `env` | Model-provider variables passed through in `trusted-local` mode. GitHub, Git, SSH, `HOME`, and `XDG_*` variables are rejected. |
| `review_network` | Allows reviewer egress in `trusted-local` mode only. |

Route roles to different harnesses per run with `--planner-agent`, `--reviewer-agent`, `--developer-agent`, and `--tester-agent`, or set project defaults with `default_agent` and the top-level `roles:` map (for example `roles: {reviewer: claude}`).

## What every harness shares

- **Login stays with the harness.** SwarmOS never reads, copies, or proxies provider credentials. When a session expires, sign in again through the harness and resume with `swarm issue work <issue>`. Completed checkpoints are kept.
- **Agents never get publishing credentials.** GitHub tokens, `SWARM_GITHUB_TOKEN_COMMAND`, Git credentials, and SSH agents stay with the `swarm` coordinator.
- **Workers see a detached snapshot.** Isolated runs get a copy of the task files without live Git metadata or coordinator state. SwarmOS imports only the expected role artifacts or bounded developer and tester changes.
- **Planner and reviewer source is read-only.** Reviewers write only `REVIEW.md` output.
- **Tool networking is off.** Model traffic goes through the harness; tool commands have no network access. Install build tools and dependencies before running work.
- **Extensions are restricted.** External MCP servers, plugins, hooks, browser tools, and subagents are disabled until each one has its own boundary tests. The [capability table](../ARCHITECTURE.md#tool-capabilities) shows the current coverage.
- **No silent fallback.** If the selected boundary is unavailable or a canary check fails, the run stops before any work is claimed. It never falls back to running on the host.
- **Provenance records what ran.** Each role attestation records the agent name, harness, qualified version, model, and sandbox.

## Execution modes and backends

| Setting | What it does |
| --- | --- |
| `execution.mode: isolated` (default) | Runs each role inside the boundary described in its harness guide. |
| `execution.mode: trusted-local` | Runs the harness CLI directly with your OS permissions and a temporary home directory. Reviewers keep their snapshot sandbox. Select it only for repositories and tools you trust, and only where `security.require_isolation` is not set. |
| `execution.backend: native` (default) | Uses the harness boundaries on this page. |
| `execution.backend: oci` | Runs offline agents (`authentication: none`) in a Docker or Podman container pinned by digest. Authenticated harnesses in OCI are on the [roadmap](../../todo.md#oci-authentication-and-enterprise-integrations). |

You can also set the mode locally with `SWARM_EXECUTION_MODE`, unless approved policy requires isolation. Native isolated execution is qualified on Linux; WSL2 uses the Linux path. Native macOS and Windows execution are not yet qualified.

On a CI runner, `swarm repo validate` runs required checks directly on the ephemeral runner, so stock GitHub runners do not need a harness installed. See [execution modes](../../SECURITY.md#execution-modes).

### Native storage lifetimes

Linux hosts can make interrupted managed-cache uses recoverable by providing a mounted cgroup v2 hierarchy, a reachable systemd user manager, and systemd 254 or newer with `systemd-run --expand-environment=no`. Start Swarm from a user session with access to that manager; for unattended workers, configure the host to keep the user manager available between sessions.

Swarm places each native command in its own scope and stops the whole scope, including detached descendants, before completing cache bookkeeping. It records the verified kernel identity and emptiness so maintenance can recover an interrupted use once the coordinator and every recorded consumer have ended. Temporary scratch and other unclassified uses remain conservative.

This setup is optional for native execution. When scope support is unavailable, native commands continue with the existing process cleanup and their storage uses stay unrecoverable (recorded as Outside). Harness sandbox prerequisites and filesystem, network, and credential restrictions still apply.

### OCI backend qualification

The container tests are opt-in. Run them on a disposable machine with a local Docker-compatible daemon and a preloaded Linux image pinned by SHA-256 digest. The image needs `/bin/sh`, `sleep`, `cat`, `chmod`, `mkdir`, `rm`, `ln`, and `dd`.

```bash
: "${SWARM_TEST_WORKER_IMAGE:?Set a preloaded image pinned by SHA-256 digest}"
: "${SWARM_TEST_WORKER_RUNTIME:?Select docker or podman}"
go test ./internal/sandbox -run '^TestWorkerOCI' -count=1
```

The fixtures check host-file and credential canaries, source protection, network isolation, unsafe output archives, background-process cleanup, interactive input, deadlines, and temporary-storage exhaustion.

## Qualifying a new harness release

The [qualification guide](../native-qualification.md) has the boundary and authenticated test commands for each harness. Before raising a minimum version or enabling a new tool surface:

1. Review the release's permission, configuration-precedence, and tool changes.
2. Run the boundary tests and an authenticated task on each platform you intend to support.
3. Add boundary tests for any new capability (MCP, subagents, hooks) before enabling it. A permission prompt alone does not count as isolation.

There is no flag to skip verification. If a harness release drops a required interface, SwarmOS refuses to run it until the adapter is updated.

## Go concurrency

Linux native isolated agent commands and worker checks default to `GOMAXPROCS=2` and `GOFLAGS=-buildvcs=false -p=2`. Bounding Go runtime and package-build concurrency helps checks run reliably on high-core machines within the existing process budget.



## Inspecting task tools

Before claiming work, inspect the tools permitted by the task's approved policy:

```bash
swarm issue work 380 --dry-run
swarm issue work 380 --dry-run --format=text
swarm repo doctor
swarm repo doctor --capabilities
swarm repo doctor --json
```

Dry-run defaults to JSON; `--format=json` selects it explicitly.
`--json` conflicts with `--format=text`. Doctor inspects the repository
configuration, while task dry-run reads the approved base. Each report labels
that policy context.

Doctor shows one capability summary line per harness/role by default, naming
each tool entry and its usable state or reason code. Declared or authorized
tools that are unusable include their action text on that line. Use
`swarm repo doctor --capabilities` for full reports. JSON output is unchanged
with or without `--capabilities`.

The shared report separates declared, authorized and runtime status. An entry
is usable only after authorization and applicable runtime verification.
`not_probed` means readiness has not been verified. Optional report assembly
failures remain informational; existing execution checks and an explicitly
enabled browser profile retain their preflight requirements.

Role bundles include coordinator-supplied tool observations. Bundles without
runtime observations say `not_probed`; launch still checks the boundary.
Read-only roles may see write tool names, but workspace writes remain denied.
Dependency preparation supplies read-only caches and resolved toolchain mounts;
it does not grant agent network access. Node capability labels and WorkUnit
`required_capabilities` continue to select workers independently of this report.
