---
description: Implement the approved .swarm/active/PLAN.md for the active SwarmOS WorkUnit
---

Read `.swarm/active/WORKUNIT.md`, `.swarm/active/PLAN.md`, `.swarm/active/TEST_PLAN.md`, and the approved instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Implement the planned changes step-by-step while strictly respecting Constraints and protected paths.
Write and run the developer-owned focused checks specified in `.swarm/active/TEST_PLAN.md`. The full trusted required policy is coordinator-owned reference, not a per-role execution list.
If something about this repository surprised you or cost you time, add it as a one-line bullet to `.swarm/active/LESSONS.md` for the next contributor.

Run only the expensive checks assigned to developer in TEST_PLAN.md. Reuse another role's recorded applicable result when the source tree is unchanged, and record the evidence reference and reuse decision. Missing evidence or unresolved applicability remains unverified. The coordinator's trusted required-check run still runs once and is never skipped because an agent reported a pass.
