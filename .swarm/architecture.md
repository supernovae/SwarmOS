# Architecture

- Contract-driven work units live in GitHub Issues and are compiled into `.swarm/active/WORKUNIT.md` per isolated worktree.
- Protected paths (`.github/workflows/**`, `.swarm/validation/**`, `.swarm/contracts/**`) are enforced by `swarm issue validate`.
