---
description: Validate the active SwarmOS WorkUnit implementation against required checks and acceptance criteria
---

Read `.swarm/active/SYSTEM_PROMPT.md`, `.swarm/active/WORKUNIT.md`, and `.swarm/active/TEST_PLAN.md`.
Independently review source and every acceptance criterion, and reuse applicable passing developer results from `.swarm/active/TEST_PLAN.md`, recording evidence references and reuse decisions. Run focused diagnostics only for relevant changes, observed failures or an unresolved coverage gap; report observed results and unverified acceptance criteria. The full trusted required policy is coordinator-owned reference, not a per-role execution list. The coordinator performs claim-bound `swarm issue validate` and publication; do not request its credentials or treat candidate policy as authoritative.
