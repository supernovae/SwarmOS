# Grok wrapper configuration

> **Unverified integration:** `grok` is not a supported native SwarmOS harness or a verified vendor CLI integration. A `grok` configuration name is only an audit/policy identity that must select one of SwarmOS's supported harness drivers and point at a binary independently proven to accept that driver's interface. No official distribution, version, authentication contract, or end-to-end behavior is asserted here. This documentation targets unreleased `main`, not necessarily v0.1.0.

Use the shared [agent integration runbook](developer.md#agent-integration-runbook) for lifecycle, credentials, sandboxing, review limits, checkpoints, and recovery. An external planning board may feed approved GitHub WorkUnits; SwarmOS governs execution state, not all product planning.

## Configure deliberately

Install and validate the client binary yourself, then pin the precise executable and a supported harness driver in repository policy. This example is configuration, not an installation instruction:

```yaml
agents:
  grok:
    binary: /absolute/path/to/grok-compatible-client
    harness: codex
    roles: [planner, reviewer]
    sandbox: sandbox-exec
```

The configured `grok` name remains the audit identity. `harness: codex` makes SwarmOS construct Codex-shaped commands (`<binary> exec <combined prompt>` headlessly and `<binary> <task>` interactively). Only `claude`, `codex`, `omp`, and `opencode` are valid harness values. A direct `grok -p` integration is not supported.

The entry must be under `agents`, be permitted for the requested role, and be available before claim. Do not use environment-variable binary overrides as a production policy bypass; use reviewed config.

## Safe operating recipe

```text
1. Create a draft WorkUnit and have an authorized maintainer approve its exact revision.
2. Inspect `swarm work <issue> --dry-run` for the resolved Grok wrapper, gate, base, and policy.
3. Run `swarm work <issue> --headless` only in a configured sandboxed environment.
4. For review, require the output-only reviewer snapshot and exact REVIEW.md verdict.
5. Validate the exact checkpoint; submit separately after gate and validation checks pass.
```

Grok receives generated `.swarm/active/WORKUNIT.md` and `SYSTEM_PROMPT.md` in the task worktree. Those files convey policy; they do not sandbox the process or make untrusted visible text harmless. Do not provide GitHub publishing credentials to the wrapper.

Before assigning real work, prove the configured binary's version, authentication, prompt behavior, output paths, and sandbox behavior in a disposable trusted repository. Record that operational evidence outside this page; SwarmOS does not currently verify it.
