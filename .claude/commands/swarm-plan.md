---
description: Execute the SwarmOS Plan phase (analyze, design PLAN.md, and produce TEST_PLAN.md)
---

Read `.swarm/active/WORKUNIT.md` and the approved role instructions in `.swarm/active/SYSTEM_PROMPT.md`.
Inspect the repository without modifying production source files.
Produce:
1. `.swarm/active/PLAN.md` with **What we know** (with evidence), **What we're assuming**, and **Open questions**, followed by exact files, symbols, and step-by-step implementation details. If WorkUnit dimensions are absent, state suggested size, uncertainty, and risk as advisory. Only the compiled approved execution profile supplies variant requirements.
2. `.swarm/active/TEST_PLAN.md` mapping every Acceptance Criterion in `.swarm/active/WORKUNIT.md` to concrete test cases and verification commands, with developer-owned focused checks, tester-owned independent review, and coordinator-owned reference policy kept separate.

End PLAN.md with a `## HANDOFF` section for the developer and tester: entry points and symbols with `path:line` references, existing tests and fixtures to extend, the exact focused verification commands, assumptions you already checked, conditions that should reopen the plan, and **What this change breaks** (existing tests, docs, templates, and call sites that depend on what the plan changes). Confirm every file the plan calls new is absent at the approved base.

Plan exactly the Acceptance Criteria. Related improvements become recommended follow-up WorkUnits, not plan steps. If a criterion needs an environment the sandbox cannot reach, list it under **Open questions** and plan the part you can verify; never drop a criterion silently. If the work cannot fit one claim, follow the split recommendation format in the approved planner prompt (`.swarm/prompts/planner.md`).

For every check expected to take more than 5 minutes, list the command/check, exactly one owner (`developer`, `tester`, `coordinator`, or `CI`), expected duration, and later roles that reuse its result instead of rerunning it. Specify its evidence location and source-tree identity/applicability.
