# Project Goals & Quality Bar

## Mission
Deliver reliable, maintainable changes that align with the repository's architecture and pass every baseline-pinned check before merge.

## Non-Negotiable Safety Invariants
- Never modify protected harness or CI files (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`, `.swarm/config.yaml`, `.swarm/prompts/**`, `.swarm/workflows/**`, `.swarm/goals.md`, `.swarm/SKILLS.md`, `.swarm/rules/**`, `.swarm/skills/**`) unless the WorkUnit explicitly sets `allow_harness_changes: true`.
- Every change must trace directly to the approved `WorkUnit` objective, constraints, and acceptance criteria.
- Prefer minimal, well-tested diffs over speculative refactoring or unrequested dependencies.

## Architectural Priorities
1. Correctness and deterministic behavior under concurrency.
2. Clear module boundaries and backwards-compatible interfaces.
3. Automated test coverage for every new code path and regression fix.
