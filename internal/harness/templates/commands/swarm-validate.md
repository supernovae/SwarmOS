---
description: Validate the active SwarmOS WorkUnit implementation against required checks and acceptance criteria
---

Read `.swarm/active/SYSTEM_PROMPT.md`, `.swarm/active/WORKUNIT.md`, and `.swarm/active/TEST_PLAN.md`.
Independently review source and every acceptance criterion, and reuse applicable passing developer results from `.swarm/active/IMPLEMENTATION.md` (including its Prior Artifacts representation), recording evidence references and reuse decisions. Run focused diagnostics only for relevant changes, observed failures or an unresolved coverage gap; report observed results and unverified acceptance criteria. The full trusted required policy is coordinator-owned reference, not a per-role execution list. The coordinator performs claim-bound `swarm issue validate` and publication; do not request its credentials or treat candidate policy as authoritative.

Run only the expensive checks assigned to tester in TEST_PLAN.md. Reuse another role's recorded applicable result when the source tree is unchanged, and record the evidence reference and reuse decision. Missing evidence or unresolved applicability remains unverified. The coordinator's trusted required-check run still runs once and is never skipped because an agent reported a pass.
