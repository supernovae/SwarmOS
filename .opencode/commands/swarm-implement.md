---
description: Implement the approved .swarm/active/PLAN.md for the active SwarmOS WorkUnit
---

Read `.swarm/active/WORKUNIT.md`, `.swarm/active/PLAN.md`, `.swarm/active/TEST_PLAN.md`, and the approved instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Implement the planned changes step-by-step while strictly respecting Constraints and protected paths.
For native issue work, if a failed plan assumption leaves the smallest correct slice short of the approved plan, write a standalone `Status: PARTIAL` line in IMPLEMENTATION.md followed by the failed assumption and unmet acceptance criteria. This preserves the checkpoint and stops for planning or WorkUnit scope review before testing.
Write and run the developer-owned focused checks specified in `.swarm/active/TEST_PLAN.md`. The full trusted required policy is coordinator-owned reference, not a per-role execution list.
If something about this repository surprised you or cost you time, add it as a one-line bullet to `.swarm/active/LESSONS.md` for the next contributor.

Run only the expensive checks assigned to developer in TEST_PLAN.md. Reuse another role's recorded applicable result when the source tree is unchanged, and record the evidence reference and reuse decision. Missing evidence or unresolved applicability remains unverified. The coordinator's trusted required-check run still runs once and is never skipped because an agent reported a pass.
