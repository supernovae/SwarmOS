---
description: Decompose a project roadmap into dependency-ordered SwarmOS WorkUnit issues or scan/import open issues against project goals
---

Read `.swarm/goals.md`, `.swarm/SKILLS.md`, `.swarm/project.md`, `.swarm/architecture.md`, and `.swarm/rules/`.

Use `swarm plan` to help the repository owner get ahead of PRs:

1. **Roadmap-to-Issues Mode (`--from <file>`)**:
   - Inspect the roadmap or design document.
   - Preview proposed `WorkUnit` issues and dependency waves:
     ```bash
     swarm plan --from <path-to-roadmap.md>
     ```
   - Create draft `WorkUnit` issues (`swarm:draft`) on the tracker:
     ```bash
     swarm plan --from <path-to-roadmap.md> --apply
     ```
   - Or create and immediately approve (`swarm:ready`) when authorized as a maintainer:
     ```bash
     swarm plan --from <path-to-roadmap.md> --apply --ready
     ```

2. **Backlog Sense-Making & Issue Import Mode (`--scan`)**:
   - Audit all open issues against `.swarm/goals.md`, `.swarm/rules/`, protected paths, and file touchpoint overlaps:
     ```bash
     swarm plan --scan
     ```
   - Apply triage labels (`swarm:ready`, `swarm:draft`, `swarm:needs-scope`, `swarm:blocked`) and optionally normalize unstructured issues into valid `WorkUnit` contracts:
     ```bash
     swarm plan --scan --apply --normalize
     ```
