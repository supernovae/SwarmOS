# SwarmOS Project Goals & Quality Bar

## Mission
Make GitHub and Git a zero-infrastructure coordination control plane where open-source communities and internal engineering teams can pool coding agents (`claude`, `codex`, `opencode`, `omp`, Grok Bot, OpenAI Dot) to ship validated, provenance-attested software together.

## Non-Negotiable Safety Invariants
- Never modify protected harness or CI files (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`, `.swarm/config.yaml`, `.swarm/prompts/**`, `.swarm/workflows/**`, `.swarm/goals.md`, `.swarm/SKILLS.md`, `.swarm/rules/**`, `.swarm/skills/**`) unless the `WorkUnit` explicitly sets `allow_harness_changes: true`.
- Always load authoritative validation policies (`required-checks.yaml`, `protected_paths`, and role prompts) from the approved `base_sha` rather than an untrusted candidate worktree.
- Execute the `reviewer` role inside an isolated read-only snapshot sandbox (`sandbox-exec`, `bwrap`, or OCI `docker`/`podman`) and bind review verdicts cryptographically to `WorkUnit`, `PLAN.md`, and `TEST_PLAN.md` SHA-256 digests.
- Keep provenance attestations and living GitHub issue comments free of local hostname, username, file path, or environment-variable PII.
- Keep user-facing documentation (`README.md`, `docs/*.md`, `SECURITY.md`) welcoming, accurate, and free of system-prompt bleed-through or invented CLI flags.

## Architectural Priorities
1. Deterministic Git compare-and-swap (`refs/heads/swarm-state/issue-<N>`) coordination and fenced lease safety under concurrency.
2. Ergonomic CLI workflows (`swarm repo plan`, `swarm issue stage`, `swarm issue work`, `swarm issue validate`, `swarm issue submit`) that support both headless worker pools and interactive human/IDE staging.
3. Comprehensive race-enabled automated test coverage (`go test -race ./...`) for every new command, contract field, and security boundary.
