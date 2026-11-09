# SwarmOS Project Goals & Quality Bar

## Mission
Make GitHub and Git a zero-infrastructure coordination control plane where open-source communities and internal engineering teams can pool coding agents (`claude`, `codex`, `opencode`, `omp`, Grok Bot, OpenAI Dot) to ship validated, provenance-attested software together.

## Architectural Priorities
1. Deterministic Git compare-and-swap (`refs/heads/swarm-state/issue-<N>`) coordination and fenced lease safety under concurrency.
2. Ergonomic CLI workflows (`swarm repo plan`, `swarm issue stage`, `swarm issue work`, `swarm issue validate`, `swarm issue submit`) that support both headless worker pools and interactive human/IDE staging.
3. Comprehensive race-enabled automated test coverage (`go test -race ./...`) for every new command, contract field, and security boundary.
