---
description: Validate the active SwarmOS WorkUnit implementation against required checks and acceptance criteria
---

Read `.swarm/active/SYSTEM_PROMPT.md`, `.swarm/active/WORKUNIT.md`, and `.swarm/active/TEST_PLAN.md`.
Independently review source and every acceptance criterion, and reuse applicable passing developer results from `.swarm/active/IMPLEMENTATION.md` (including its Prior Artifacts representation), recording evidence references and reuse decisions. Run focused diagnostics only for relevant changes, observed failures or an unresolved coverage gap; report observed results and unverified acceptance criteria. The full trusted required policy is coordinator-owned reference, not a per-role execution list. The coordinator performs claim-bound `swarm issue validate` and publication; do not request its credentials or treat candidate policy as authoritative.

Run only the expensive checks assigned to tester in TEST_PLAN.md. Reuse another role's recorded applicable result when the source tree is unchanged, and record the evidence reference and reuse decision. Missing evidence or unresolved applicability remains unverified. The coordinator's trusted required-check run still runs once and is never skipped because an agent reported a pass.

## Acceptance evidence

Record results in `.swarm/active/VALIDATION.md` and include exactly one block with standalone delimiters and JSON without code fences; `swarm issue submit` refuses a missing or malformed block:

<!-- swarm:acceptance-evidence:start -->
{"version":1,"criteria":[{"index":1,"criterion":"Exact acceptance text","status":"met","evidence":"IMPLEMENTATION.md: Results: before 12s, after 8s"}]}
<!-- swarm:acceptance-evidence:end -->

Use one-based indices in approved WorkUnit acceptance order and copy the exact criterion text without Markdown checkbox prefixes. Include every criterion with status `met`, `unmet` or `external`. Use `met` only with a nonblank evidence reference; unverified criteria stay `unmet`. Use `external` only for an environment agents cannot reach, with a nonblank `reason` naming it. Keep the block within 64 KiB.
