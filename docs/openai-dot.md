# Codex and Dot wrapper configuration

This page shares the [agent integration runbook](developer.md#agent-integration-runbook) for policy, lifecycle, sandboxing, credentials, reviewer behavior, and recovery. Documentation targets unreleased `main`; v0.1.0 may have a different command surface.

## Codex

Codex is a configured harness when its executable is listed in `.swarm/config.yaml` and allowed for the requested role:

```yaml
agents:
  codex:
    binary: codex
    roles: [planner, developer]
```

SwarmOS constructs `codex exec <combined prompt>` in headless mode and `codex <task>` in interactive mode. It checks the executable before claim, but this is not proof of account access, model entitlement, billing, or a sandbox. Configure an isolated reviewer environment and explicitly scoped model-authentication environment where needed.

## Dot

> **Unverified integration:** `dot` is not a supported native SwarmOS harness and is not a confirmed official OpenAI CLI contract. A `dot` configuration name must select one of the supported harness drivers and point at a binary independently proven to accept that driver's interface. SwarmOS makes no vendor compatibility guarantee.

Only configure Dot after choosing and independently testing a concrete distribution/version and compatible harness contract:

```yaml
agents:
  dot:
    binary: /absolute/path/to/dot-compatible-client
    harness: codex
    roles: [reviewer]
    sandbox: sandbox-exec
```

`harness: codex` constructs Codex-shaped commands (`<binary> exec <combined prompt>` headlessly and `<binary> <task>` interactively). Valid drivers are only `claude`, `codex`, `omp`, and `opencode`; a direct `dot exec` integration is not supported. The configured path, allowed roles, optional arguments/model, environment allowlist, and sandbox are repository policy. A wrapper that is installed but absent from `agents` is intentionally unavailable.

## Plan-bound review recipe

```bash
# Show the exact effective policy before work; this does not claim or run an agent.
swarm work 42 --dry-run

# A configured reviewer is used only for an agent gate.
swarm work 42 --gate agent --reviewer-agent codex --headless
```

A reviewer operates on a separate immutable snapshot and may import only `REVIEW.md`. It must emit exactly `VERDICT: APPROVED` or `VERDICT: CHANGES_REQUESTED`. Approval is bound to current WorkUnit, plan, and test-plan hashes. A reviewer cannot satisfy a `human` gate; a permitted human records that decision with `swarm review 42 --approve` or `--reject`.

Do not use provider labels as an authorization system. State authority remains Git CAS, publication credentials remain with the coordinator, and final merge authority remains repository branch protection and human PR review.

An external task board can create or link to approved GitHub WorkUnits, but it does not replace the WorkUnit digest, claim fencing, checkpoint, gate, or validation evidence.
